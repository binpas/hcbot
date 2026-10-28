# GPU worker for /say (text to speech) and the Markov ranking (linux/amd64,
# NVIDIA GPU). See README.md, "Text to speech".
#
# The image is large (about 8 GB, most of it PyTorch and CUDA), so it is
# built on the GPU host and is not put on GHCR:
#   docker build -t hcbot-tts:local tts/
#
# The image has no secrets. The models (about 7 GB) download into /data/hf
# at the first start, at the fixed revisions in server.py.

FROM pytorch/pytorch:2.9.1-cuda12.8-cudnn9-runtime@sha256:7b324d212a4450795b49edba9949b7cdc72429148a64e974334bfe5774d51385

# torchcodec (audio decoding) needs the FFmpeg libraries.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir \
        torch==2.9.1 torchaudio==2.9.1 torchcodec==0.8.1 \
        huggingface_hub==2.3.0 hf-xet==1.7.0 safetensors==0.8.0 einops==0.8.2 \
        transformers==5.21.0 tokenizers==0.23.3

# Echo-TTS code at a fixed commit. The checksum makes sure that the
# download is the expected one.
ARG ECHO_COMMIT=2ed95fce62d33bf7b56f835fd9ec0f0b6fb9155e
ARG ECHO_SHA256=97113973cee2044f3ec4a7fdd72cc6c99ab3238f9316916d25c4223b0a2bcbf1
WORKDIR /app
RUN curl -fsSL -o echo.tar.gz \
        "https://github.com/jordandare/echo-tts/archive/${ECHO_COMMIT}.tar.gz" \
 && echo "${ECHO_SHA256}  echo.tar.gz" | sha256sum -c - \
 && tar xzf echo.tar.gz --strip-components=1 \
 && rm echo.tar.gz
COPY server.py dashboard.html /app/

# UID and GID 568 are the TrueNAS "apps" user.
USER 568:568
ENV HF_HOME=/data/hf HOME=/tmp PYTHONUNBUFFERED=1
EXPOSE 8765 8766
STOPSIGNAL SIGTERM
CMD ["python", "/app/server.py"]
