"""GPU worker for the HEALTH bot: text to speech (/say) and Markov ranking.

It runs Echo-TTS (https://github.com/jordandare/echo-tts) for speech, and
a small language model (Qwen 2.5 0.5B Instruct) that gives Markov chat
candidates a score. It answers the bot over HTTP. The models stay loaded.
One clip is made at a time, and one ranking; the two can run together.
See README.md, "Text to speech" and "Markov chat".

Endpoints (all except /health need "Authorization: Bearer <TTS_TOKEN>"):
  GET  /health   {"ok": true, "voices": n, "rank": true}
  GET  /voices   {"voices": ["alex", "sam"]}
  POST /reload   reads the voices folder again
  POST /tts      {"text": "...", "voice": "alex"} -> audio/wav
  POST /rank     {"context": "...", "candidates": ["...", ...]}
                 -> {"scores": [...]}, higher is better

A voice is an audio file in VOICES_DIR (wav, mp3, flac, ogg, m4a). The
file name is the voice name. An optional <name>.json next to it can turn
on Force Speaker: {"force_speaker": 1.5}.

The dashboard is a second HTTP server on DASHBOARD_PORT (0 turns it off),
for the operator. It shows the queued, running, and last finished clips
with their text. With DASHBOARD_TOKEN set, it asks for that token as the
password (any user name). It does not show the Markov rankings.
  GET  /           the page (dashboard.html)
  GET  /api/jobs   the jobs and the counts as JSON
  GET  /clips/<id> the saved WAV of a recent job (?download=1 to save it)

When CLIPS_DIR is set, the worker saves each clip there for the operator,
as <date>/<time>-<id>_<voice>_<text>.wav with a .json next to it (the
text, the voice, and the times). CLIPS_KEEP_DAYS > 0 deletes the date
folders that are older.

The worker does not log the text. The jobs list is only in memory.
"""

import base64
import collections
import datetime
import functools
import hmac
import io
import json
import logging
import os
import re
import secrets
import threading
import time
import wave
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import torch
from transformers import AutoModelForCausalLM, AutoTokenizer

import inference
from inference import (
    load_audio,
    load_fish_ae_from_hf,
    load_model_from_hf,
    load_pca_state_from_hf,
    sample_euler_cfg_independent_guidances,
    sample_pipeline,
)

log = logging.getLogger("tts")

TOKEN = os.environ.get("TTS_TOKEN", "")
VOICES_DIR = os.environ.get("VOICES_DIR", "/voices")
PORT = int(os.environ.get("PORT", "8765"))
STEPS = int(os.environ.get("TTS_STEPS", "40"))
DTYPE = {"bf16": torch.bfloat16, "fp16": torch.float16}[os.environ.get("TTS_DTYPE", "bf16")]
# Text up to SHORT_CHARS characters uses the half length (about 15 s of
# audio), which is twice as fast.
SHORT_CHARS = int(os.environ.get("TTS_SHORT_CHARS", "150"))
MAX_CHARS = 600
MAX_BODY = 16 << 10

DASHBOARD_PORT = int(os.environ.get("DASHBOARD_PORT", "8766"))
DASHBOARD_TOKEN = os.environ.get("DASHBOARD_TOKEN", "")
DASHBOARD_HTML = os.path.join(os.path.dirname(os.path.abspath(__file__)), "dashboard.html")
RECENT_JOBS = 50
CLIPS_DIR = os.environ.get("CLIPS_DIR", "")
CLIPS_KEEP_DAYS = int(os.environ.get("CLIPS_KEEP_DAYS", "0"))
DATE_DIR_RE = re.compile(r"^\d{4}-\d{2}-\d{2}$")

# The model versions are fixed, so an upload to Hugging Face cannot change
# the worker without a rebuild.
REVISIONS = {
    "jordand/echo-tts-base": "06731f67000241d55cff6b0a961f22f5f151f24f",
    "jordand/fish-s1-dac-min": "18b770e5b62b1a58ebd7423401a937cc063d8729",
}
_hf_download = inference.hf_hub_download


def _pinned_download(repo_id, filename, **kw):
    return _hf_download(repo_id, filename, revision=REVISIONS[repo_id], **kw)


inference.hf_hub_download = _pinned_download

# The ranking model, at a fixed revision. RANK_ENABLED=0 does not load it.
RANK_MODEL = "Qwen/Qwen2.5-0.5B-Instruct"
RANK_REVISION = "7ae557604adf67be50417f59c2c2f167def9a775"
RANK_ENABLED = os.environ.get("RANK_ENABLED", "1") != "0"
RANK_MAX_CANDIDATES = 20
RANK_MAX_CHARS = 2000
RANK_SYSTEM = "You are a member of a casual Discord chat server. Reply with one short chat message."
RANK_NO_CONTEXT = "Say something in the chat."

AUDIO_EXTS = {".wav", ".mp3", ".flac", ".ogg", ".m4a"}
VOICE_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,31}$")
SAMPLE_RATE = 44100


class Voices:
    """The reference clips, decoded once and kept in memory."""

    def __init__(self, folder):
        self.folder = folder
        self.lock = threading.Lock()
        self.voices = {}

    def reload(self):
        found = {}
        try:
            names = sorted(os.listdir(self.folder))
        except OSError as e:
            log.warning("voices folder: %s", e)
            names = []
        for fname in names:
            stem, ext = os.path.splitext(fname)
            name = stem.lower()
            if ext.lower() not in AUDIO_EXTS:
                continue
            if not VOICE_NAME_RE.match(name) or name == "random":
                log.warning("voice %r skipped: use a-z, 0-9, - and _ (at most 32)", fname)
                continue
            if name in found:
                log.warning("voice %r skipped: two files have the name %r", fname, name)
                continue
            try:
                audio = load_audio(os.path.join(self.folder, fname), max_duration=60)
            except Exception as e:  # a bad file must not stop the worker
                log.warning("voice %r skipped: %s", fname, e)
                continue
            opts = {}
            side = os.path.join(self.folder, stem + ".json")
            if os.path.exists(side):
                try:
                    with open(side) as f:
                        opts = json.load(f)
                except (OSError, ValueError) as e:
                    log.warning("voice %r: bad %s.json: %s", fname, stem, e)
            found[name] = (audio, opts)
        with self.lock:
            self.voices = found
        log.info("loaded %d voices: %s", len(found), ", ".join(found) or "none")
        return len(found)

    def names(self):
        with self.lock:
            return sorted(self.voices)

    def get(self, name):
        with self.lock:
            return self.voices.get(name)


class Engine:
    def __init__(self):
        self.model = load_model_from_hf(dtype=DTYPE, delete_blockwise_modules=True)
        self.fish_ae = load_fish_ae_from_hf()
        self.pca_state = load_pca_state_from_hf()
        self.gpu = threading.Lock()

    def speak(self, text, voice, on_start=None):
        """Returns mono 16-bit WAV bytes and the length in seconds. voice is
        None or (audio, opts). on_start is called when the GPU is free and
        the clip starts."""
        seq = 320 if len(text) <= SHORT_CHARS else 640
        speaker, kv = None, {}
        if voice is not None:
            audio, opts = voice
            speaker = audio.cuda()
            if opts.get("force_speaker"):
                kv = {
                    "speaker_kv_scale": float(opts["force_speaker"]),
                    "speaker_kv_min_t": float(opts.get("force_speaker_min_t", 0.9)),
                    "speaker_kv_max_layers": int(opts.get("force_speaker_max_layers", 24)),
                }
        sample_fn = functools.partial(
            sample_euler_cfg_independent_guidances,
            num_steps=STEPS,
            cfg_scale_text=3.0,
            cfg_scale_speaker=8.0,
            cfg_min_t=0.5,
            cfg_max_t=1.0,
            truncation_factor=None,
            rescale_k=None,
            rescale_sigma=None,
            speaker_kv_scale=kv.get("speaker_kv_scale"),
            speaker_kv_max_layers=kv.get("speaker_kv_max_layers"),
            speaker_kv_min_t=kv.get("speaker_kv_min_t"),
            sequence_length=seq,
        )
        with self.gpu, torch.inference_mode():
            if on_start:
                on_start()
            out, _ = sample_pipeline(
                model=self.model,
                fish_ae=self.fish_ae,
                pca_state=self.pca_state,
                sample_fn=sample_fn,
                text_prompt="[S1] " + text,
                speaker_audio=speaker,
                rng_seed=secrets.randbits(31),
            )
            samples = out[0].float().cpu().numpy().reshape(-1)
        samples = np.nan_to_num(samples)
        pcm = (np.clip(samples, -1.0, 1.0) * 32767).astype("<i2")
        buf = io.BytesIO()
        with wave.open(buf, "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(SAMPLE_RATE)
            w.writeframes(pcm.tobytes())
        return buf.getvalue(), len(pcm) / SAMPLE_RATE


class Jobs:
    """The clip jobs for the dashboard: the queued and running ones, and the
    last RECENT_JOBS finished ones, with counts since the start. Only in
    memory."""

    def __init__(self):
        self.lock = threading.Lock()
        self.next_id = 1
        self.active = {}
        self.recent = collections.deque(maxlen=RECENT_JOBS)
        self.started = time.time()
        self.done = 0
        self.failed = 0
        self.audio_seconds = 0.0
        self.gen_seconds = 0.0

    def add(self, text, voice):
        with self.lock:
            job = {"id": self.next_id, "text": text, "voice": voice, "state": "queued",
                   "received": time.time(), "started": None, "finished": None,
                   "audio_seconds": None, "gen_seconds": None, "clip": None}
            self.next_id += 1
            self.active[job["id"]] = job
        return job

    def start(self, job):
        with self.lock:
            job["state"] = "running"
            job["started"] = time.time()

    def finish(self, job, audio_seconds=None, gen_seconds=None, clip=None):
        """Without audio_seconds, the job failed."""
        with self.lock:
            self.active.pop(job["id"], None)
            job["finished"] = time.time()
            if audio_seconds is None:
                job["state"] = "failed"
                self.failed += 1
            else:
                job.update(state="done", audio_seconds=audio_seconds,
                           gen_seconds=gen_seconds, clip=clip)
                self.done += 1
                self.audio_seconds += audio_seconds
                self.gen_seconds += gen_seconds
            self.recent.appendleft(job)

    def clip(self, job_id):
        with self.lock:
            for job in self.recent:
                if job["id"] == job_id:
                    return job["clip"]
        return None

    def snapshot(self):
        def pub(job):
            return {k: v for k, v in job.items() if k != "clip"} | {"clip": bool(job["clip"])}

        with self.lock:
            active = sorted(self.active.values(), key=lambda j: j["id"])
            return {
                "uptime": time.time() - self.started,
                "done": self.done,
                "failed": self.failed,
                "audio_seconds": self.audio_seconds,
                "gen_seconds": self.gen_seconds,
                "running": [pub(j) for j in active if j["state"] == "running"],
                "queued": [pub(j) for j in active if j["state"] == "queued"],
                "recent": [pub(j) for j in self.recent],
            }


class Clips:
    """Saves the clips in CLIPS_DIR, one folder for each day (UTC)."""

    def __init__(self, folder, keep_days):
        self.folder = folder
        self.keep_days = keep_days
        self.lock = threading.Lock()

    def save(self, job, wav, audio_seconds):
        """Returns the path of the WAV, or None if it was not saved."""
        now = datetime.datetime.now(datetime.timezone.utc)
        day = os.path.join(self.folder, now.strftime("%Y-%m-%d"))
        slug = re.sub(r"[^a-z0-9]+", "-", job["text"].lower())[:40].strip("-")
        stem = f"{now:%H%M%S}-{job['id']}_{job['voice']}" + (f"_{slug}" if slug else "")
        path = os.path.join(day, stem + ".wav")
        meta = {"text": job["text"], "voice": job["voice"], "audio_seconds": round(audio_seconds, 2),
                "created": now.isoformat(timespec="seconds")}
        try:
            os.makedirs(day, exist_ok=True)
            _write_file(path, wav)
            _write_file(os.path.join(day, stem + ".json"),
                        json.dumps(meta, ensure_ascii=False, indent=2).encode())
        except OSError as e:
            log.warning("clip not saved: %s", e)
            return None
        self.prune(now)
        return path

    def prune(self, now):
        if self.keep_days <= 0:
            return
        oldest = (now - datetime.timedelta(days=self.keep_days)).strftime("%Y-%m-%d")
        with self.lock:
            try:
                days = os.listdir(self.folder)
            except OSError:
                return
            for name in days:
                if not DATE_DIR_RE.match(name) or name >= oldest:
                    continue
                folder = os.path.join(self.folder, name)
                try:
                    # Only our files; a folder with other files stays.
                    for f in os.listdir(folder):
                        if f.endswith((".wav", ".json")):
                            os.remove(os.path.join(folder, f))
                    os.rmdir(folder)
                    log.info("deleted the clips of %s", name)
                except OSError as e:
                    log.warning("clips of %s not deleted: %s", name, e)


def _write_file(path, data):
    """Writes the file in full, or not at all."""
    tmp = path + ".part"
    with open(tmp, "wb") as f:
        f.write(data)
    os.replace(tmp, path)


class Ranker:
    """Gives each candidate a score: the mean log probability of its tokens
    as the model's reply to the context. Fluent text that fits the context
    gets a higher score."""

    def __init__(self):
        self.tok = AutoTokenizer.from_pretrained(RANK_MODEL, revision=RANK_REVISION)
        # The 2080 Ti (Turing) has no fast bfloat16, so float16.
        self.model = AutoModelForCausalLM.from_pretrained(
            RANK_MODEL, revision=RANK_REVISION, dtype=torch.float16
        ).cuda().eval()
        self.lock = threading.Lock()

    def score(self, context, candidates):
        msgs = [
            {"role": "system", "content": RANK_SYSTEM},
            {"role": "user", "content": context or RANK_NO_CONTEXT},
        ]
        text = self.tok.apply_chat_template(msgs, tokenize=False, add_generation_prompt=True)
        prefix = self.tok(text, add_special_tokens=False).input_ids
        scores = []
        with self.lock, torch.inference_mode():
            for cand in candidates:
                # The reply and the end-of-turn token, after the prompt.
                reply = self.tok(cand + "<|im_end|>", add_special_tokens=False).input_ids
                ids = prefix + reply
                x = torch.tensor([ids], device="cuda")
                logits = self.model(x).logits[0, :-1].float()
                logp = torch.log_softmax(logits, dim=-1)
                target = x[0, 1:]
                tok_logp = logp[torch.arange(len(target), device="cuda"), target]
                # Only the reply's tokens (with the end-of-turn token).
                scores.append(float(tok_logp[len(prefix) - 1 :].mean()))
        return scores


def clean_text(text):
    """Removes speaker tags and control characters. Commas are pauses."""
    text = re.sub(r"[\[\]]", "", text)
    text = "".join(ch if ch.isprintable() else " " for ch in text)
    return " ".join(text.split())


class Handler(BaseHTTPRequestHandler):
    server_version = "hcbot-tts"

    def log_message(self, fmt, *args):
        # Only the method, the path, and the status. Never the text.
        log.info("%s %s", self.address_string(), fmt % args)

    def send_json(self, status, obj):
        body = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def authorized(self):
        got = self.headers.get("Authorization", "")
        if TOKEN and hmac.compare_digest(got.encode(), ("Bearer " + TOKEN).encode()):
            return True
        self.send_json(401, {"error": "unauthorized"})
        return False

    def do_GET(self):
        if self.path == "/health":
            self.send_json(200, {"ok": True, "voices": len(self.server.voices.names()),
                                 "rank": self.server.ranker is not None})
        elif self.path == "/voices":
            if self.authorized():
                self.send_json(200, {"voices": self.server.voices.names()})
        else:
            self.send_json(404, {"error": "not found"})

    def do_POST(self):
        if self.path not in ("/tts", "/reload", "/rank"):
            self.send_json(404, {"error": "not found"})
            return
        if not self.authorized():
            return
        if self.path == "/reload":
            self.send_json(200, {"voices": self.server.voices.reload()})
            return
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0 or length > MAX_BODY:
            self.send_json(400, {"error": "bad body"})
            return
        if self.path == "/rank":
            self.rank(self.rfile.read(length))
            return
        try:
            req = json.loads(self.rfile.read(length))
            text = clean_text(str(req.get("text", "")))
            voice_name = str(req.get("voice") or "random").lower()
        except (ValueError, AttributeError):
            self.send_json(400, {"error": "bad json"})
            return
        if not text or len(text) > MAX_CHARS:
            self.send_json(400, {"error": f"text must have 1 to {MAX_CHARS} characters"})
            return
        voice = None
        if voice_name != "random":
            voice = self.server.voices.get(voice_name)
            if voice is None:
                self.send_json(404, {"error": "unknown voice"})
                return
        jobs = self.server.jobs
        job = jobs.add(text, voice_name)
        try:
            data, audio_seconds = self.server.engine.speak(text, voice, lambda: jobs.start(job))
        except Exception:
            jobs.finish(job)
            log.exception("speak failed")
            self.send_json(500, {"error": "speak failed"})
            return
        gen_seconds = time.time() - job["started"]
        try:
            self.send_response(200)
            self.send_header("Content-Type", "audio/wav")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        finally:
            # Also when the bot went away: the clip is made.
            clip = None
            if self.server.clips is not None:
                clip = self.server.clips.save(job, data, audio_seconds)
            jobs.finish(job, audio_seconds, gen_seconds, clip)


    def rank(self, body):
        if self.server.ranker is None:
            self.send_json(503, {"error": "ranking is off"})
            return
        try:
            req = json.loads(body)
            context = str(req.get("context") or "")[:RANK_MAX_CHARS]
            cands = [str(c)[:RANK_MAX_CHARS] for c in req.get("candidates", [])]
        except (ValueError, AttributeError, TypeError):
            self.send_json(400, {"error": "bad json"})
            return
        if not cands or len(cands) > RANK_MAX_CANDIDATES:
            self.send_json(400, {"error": f"give 1 to {RANK_MAX_CANDIDATES} candidates"})
            return
        try:
            scores = self.server.ranker.score(context, cands)
        except Exception:
            log.exception("rank failed")
            self.send_json(500, {"error": "rank failed"})
            return
        self.send_json(200, {"scores": scores})


class DashboardHandler(BaseHTTPRequestHandler):
    server_version = "hcbot-tts"

    def log_message(self, fmt, *args):
        pass  # the page asks every 2 seconds; log_error logs the errors

    def log_error(self, fmt, *args):
        log.info("dashboard %s %s", self.address_string(), fmt % args)

    def send_body(self, status, ctype, body, extra=()):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        for k, v in extra:
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def authorized(self):
        if not DASHBOARD_TOKEN:
            return True
        got = self.headers.get("Authorization", "")
        if got.startswith("Basic "):
            try:
                _, _, password = base64.b64decode(got[6:]).decode().partition(":")
            except (ValueError, UnicodeDecodeError):
                password = ""
            if hmac.compare_digest(password.encode(), DASHBOARD_TOKEN.encode()):
                return True
        self.send_body(401, "text/plain", b"unauthorized",
                       [("WWW-Authenticate", 'Basic realm="hcbot-tts"')])
        return False

    def do_GET(self):
        if not self.authorized():
            return
        path, _, query = self.path.partition("?")
        srv = self.server.worker
        if path == "/":
            self.send_body(200, "text/html; charset=utf-8", srv.html)
        elif path == "/api/jobs":
            gpu = None
            try:
                free, total = torch.cuda.mem_get_info()
                gpu = {"used": total - free, "total": total}
            except Exception:
                pass
            body = srv.jobs.snapshot() | {
                "now": time.time(), "voices": len(srv.voices.names()),
                "rank": srv.ranker is not None, "saving": srv.clips is not None, "gpu": gpu,
            }
            self.send_body(200, "application/json", json.dumps(body).encode())
        elif path.startswith("/clips/") and path[7:].isdigit():
            clip = srv.jobs.clip(int(path[7:]))
            try:
                with open(clip, "rb") as f:
                    data = f.read()
            except (OSError, TypeError):
                self.send_body(404, "text/plain", b"not found")
                return
            extra = []
            if query == "download=1":
                extra.append(("Content-Disposition",
                              f'attachment; filename="{os.path.basename(clip)}"'))
            self.send_body(200, "audio/wav", data, extra)
        else:
            self.send_body(404, "text/plain", b"not found")


def main():
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    # httpx logs one line for each Hugging Face request, and a library sets
    # its level again while the models load, so filter its info lines.
    for h in logging.getLogger().handlers:
        h.addFilter(lambda r: not (r.name.startswith("httpx") and r.levelno < logging.WARNING))
    if len(TOKEN) < 32:
        raise SystemExit("TTS_TOKEN must be set (at least 32 characters)")
    log.info("loading the model (%s, %d steps)", os.environ.get("TTS_DTYPE", "bf16"), STEPS)
    engine = Engine()
    voices = Voices(VOICES_DIR)
    voices.reload()
    ranker = None
    if RANK_ENABLED:
        log.info("loading the ranking model")
        ranker = Ranker()
    srv = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    srv.engine, srv.voices, srv.ranker = engine, voices, ranker
    srv.jobs = Jobs()
    srv.clips = Clips(CLIPS_DIR, CLIPS_KEEP_DAYS) if CLIPS_DIR else None
    if srv.clips:
        log.info("saving the clips in %s", CLIPS_DIR)
    if DASHBOARD_PORT:
        with open(DASHBOARD_HTML, "rb") as f:
            srv.html = f.read()
        dash = ThreadingHTTPServer(("0.0.0.0", DASHBOARD_PORT), DashboardHandler)
        dash.worker = srv
        threading.Thread(target=dash.serve_forever, daemon=True).start()
        log.info("dashboard on port %d%s", DASHBOARD_PORT,
                 "" if DASHBOARD_TOKEN else " (no DASHBOARD_TOKEN: no login)")
    log.info("listening on port %d", PORT)
    srv.serve_forever()


if __name__ == "__main__":
    main()
