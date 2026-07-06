"""ASR parser sidecar: faster-whisper behind the Parser Provider HTTP surface.

Endpoints:
  GET  /healthz        -> {"status": "ok"}
  GET  /capabilities   -> parser-provider.schema.json document
  POST /parse          -> multipart file upload; returns transcription segments

The response is an AtlasDocument fragment: parser-gateway owns composing the
full AtlasDocument, storing it in object storage, and generating evidence
pointers. This sidecar never persists uploads beyond its temp directory.
"""

import os
import tempfile
import uuid

from fastapi import FastAPI, File, Form, UploadFile

app = FastAPI(title="agentatlas-asr-sidecar")

MODEL_NAME = os.environ.get("WHISPER_MODEL", "small")
DEVICE = os.environ.get("WHISPER_DEVICE", "auto")
COMPUTE_TYPE = os.environ.get("WHISPER_COMPUTE", "default")

_model = None


def get_model():
    global _model
    if _model is None:
        from faster_whisper import WhisperModel

        _model = WhisperModel(MODEL_NAME, device=DEVICE, compute_type=COMPUTE_TYPE)
    return _model


@app.get("/healthz")
def healthz():
    return {"status": "ok", "provider_id": "asr", "model": MODEL_NAME}


@app.get("/capabilities")
def capabilities():
    return {
        "provider_id": "asr",
        "capabilities": ["audio.asr"],
        "input_types": ["audio/wav", "audio/mpeg", "audio/mp4", "audio/x-m4a", "video/mp4"],
        "output_schema": "atlas_document.v1",
        "max_file_size_mb": 500,
        "supports_private": True,
    }


@app.post("/parse")
async def parse(file: UploadFile = File(...), artifact_id: str = Form("")):
    suffix = os.path.splitext(file.filename or "")[1] or ".wav"
    with tempfile.NamedTemporaryFile(suffix=suffix, delete=False) as tmp:
        tmp.write(await file.read())
        path = tmp.name
    try:
        segments, info = get_model().transcribe(path, vad_filter=True)
        audio_segments = []
        for seg in segments:
            audio_segments.append(
                {
                    "segment_id": f"as_{uuid.uuid4().hex[:12]}",
                    "start_ms": int(seg.start * 1000),
                    "end_ms": int(seg.end * 1000),
                    "text": seg.text.strip(),
                }
            )
        return {
            "provider_id": "asr",
            "artifact_id": artifact_id,
            "content_type": file.content_type,
            "language": info.language,
            "language_probability": info.language_probability,
            "duration_ms": int(info.duration * 1000),
            "audio_segments": audio_segments,
        }
    finally:
        os.unlink(path)
