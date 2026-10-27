"""Text to speech worker for the HEALTH bot (/say).

It runs Echo-TTS (https://github.com/jordandare/echo-tts) on the GPU and
answers the bot over HTTP. The model stays loaded, and one clip is made at
a time. See README.md, "Text to speech".

Endpoints (all except /health need "Authorization: Bearer <TTS_TOKEN>"):
  GET  /health   {"ok": true, "voices": n}
  GET  /voices   {"voices": ["alex", "sam"]}
  POST /reload   reads the voices folder again
  POST /tts      {"text": "...", "voice": "alex"} -> audio/wav

A voice is an audio file in VOICES_DIR (wav, mp3, flac, ogg, m4a). The
file name is the voice name. An optional <name>.json next to it can turn
on Force Speaker: {"force_speaker": 1.5}.

The worker does not log or keep the text.
"""

import functools
import hmac
import io
import json
import logging
import os
import re
import secrets
import threading
import wave
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import torch

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

    def speak(self, text, voice):
        """Returns mono 16-bit WAV bytes. voice is None or (audio, opts)."""
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
        return buf.getvalue()


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
            self.send_json(200, {"ok": True, "voices": len(self.server.voices.names())})
        elif self.path == "/voices":
            if self.authorized():
                self.send_json(200, {"voices": self.server.voices.names()})
        else:
            self.send_json(404, {"error": "not found"})

    def do_POST(self):
        if self.path not in ("/tts", "/reload"):
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
        try:
            data = self.server.engine.speak(text, voice)
        except Exception:
            log.exception("speak failed")
            self.send_json(500, {"error": "speak failed"})
            return
        self.send_response(200)
        self.send_header("Content-Type", "audio/wav")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def main():
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    if len(TOKEN) < 32:
        raise SystemExit("TTS_TOKEN must be set (at least 32 characters)")
    log.info("loading the model (%s, %d steps)", os.environ.get("TTS_DTYPE", "bf16"), STEPS)
    engine = Engine()
    voices = Voices(VOICES_DIR)
    voices.reload()
    srv = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    srv.engine, srv.voices = engine, voices
    log.info("listening on port %d", PORT)
    srv.serve_forever()


if __name__ == "__main__":
    main()
