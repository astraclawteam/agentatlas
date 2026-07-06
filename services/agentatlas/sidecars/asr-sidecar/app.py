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
# Optional diarization: WHISPER_DIARIZE=1 + pyannote available (HF_TOKEN for
# gated models). Unavailable => graceful single-speaker fallback ("S1").
DIARIZE = os.environ.get("WHISPER_DIARIZE", "0") == "1"

_model = None
_diarizer = None
_diarize_error = None


def get_model():
    global _model
    if _model is None:
        from faster_whisper import WhisperModel

        _model = WhisperModel(MODEL_NAME, device=DEVICE, compute_type=COMPUTE_TYPE)
    return _model


def get_diarizer():
    """Load the pyannote pipeline once; record the failure reason if any."""
    global _diarizer, _diarize_error
    if _diarizer is None and _diarize_error is None:
        try:
            from pyannote.audio import Pipeline

            _diarizer = Pipeline.from_pretrained(
                os.environ.get("DIARIZE_MODEL", "pyannote/speaker-diarization-3.1"),
                use_auth_token=os.environ.get("HF_TOKEN") or None,
            )
        except Exception as exc:  # noqa: BLE001 — degrade, report in payload
            _diarize_error = str(exc)
    return _diarizer


def assign_speakers(path, audio_segments):
    """Label each transcript segment with the dominant diarized speaker.

    Returns (segments, diarization_state). Fallback labels everything S1.
    """
    if not DIARIZE:
        for seg in audio_segments:
            seg["speaker"] = "S1"
        return audio_segments, "disabled"
    pipeline = get_diarizer()
    if pipeline is None:
        for seg in audio_segments:
            seg["speaker"] = "S1"
        return audio_segments, f"unavailable: {_diarize_error}"
    diarization = pipeline(path)
    turns = [
        (turn.start * 1000, turn.end * 1000, speaker)
        for turn, _, speaker in diarization.itertracks(yield_label=True)
    ]
    for seg in audio_segments:
        overlaps = {}
        for start_ms, end_ms, speaker in turns:
            ov = min(seg["end_ms"], end_ms) - max(seg["start_ms"], start_ms)
            if ov > 0:
                overlaps[speaker] = overlaps.get(speaker, 0) + ov
        seg["speaker"] = max(overlaps, key=overlaps.get) if overlaps else "S1"
    return audio_segments, "on"


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
        audio_segments, diarization = assign_speakers(path, audio_segments)
        return {
            "provider_id": "asr",
            "artifact_id": artifact_id,
            "content_type": file.content_type,
            "language": info.language,
            "language_probability": info.language_probability,
            "duration_ms": int(info.duration * 1000),
            "diarization": diarization,
            "audio_segments": audio_segments,
        }
    finally:
        os.unlink(path)
