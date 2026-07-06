"""Video parser sidecar: ffmpeg + PySceneDetect behind the Parser Provider
HTTP surface.

Endpoints:
  GET  /healthz        -> {"status": "ok"}
  GET  /capabilities   -> parser-provider.schema.json document
  POST /parse          -> multipart upload; extracts audio, detects scenes,
                          exports one keyframe per scene into /shared

Outputs land under /shared/{job_id}/ (a volume shared with parser-gateway):
  audio.wav, scene_<n>.jpg. The gateway forwards audio.wav to the ASR sidecar,
  keyframes to OCR/VLM, moves everything into object storage, and composes the
  final AtlasDocument. This sidecar never talks to object storage itself.
"""

import os
import subprocess
import tempfile
import uuid

from fastapi import FastAPI, File, Form, UploadFile

app = FastAPI(title="agentatlas-video-sidecar")

SHARED_DIR = os.environ.get("SHARED_DIR", "/shared")


@app.get("/healthz")
def healthz():
    return {"status": "ok", "provider_id": "video"}


@app.get("/capabilities")
def capabilities():
    return {
        "provider_id": "video",
        "capabilities": ["video.scene"],
        "input_types": ["video/mp4", "video/quicktime", "video/x-matroska"],
        "output_schema": "atlas_document.v1",
        "max_file_size_mb": 2048,
        "supports_private": True,
    }


def run(cmd: list[str]) -> None:
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise RuntimeError(f"{cmd[0]} failed: {proc.stderr[-2000:]}")


@app.post("/parse")
async def parse(file: UploadFile = File(...), artifact_id: str = Form("")):
    from scenedetect import ContentDetector, detect

    job_id = f"vid_{uuid.uuid4().hex[:12]}"
    out_dir = os.path.join(SHARED_DIR, job_id)
    os.makedirs(out_dir, exist_ok=True)

    suffix = os.path.splitext(file.filename or "")[1] or ".mp4"
    with tempfile.NamedTemporaryFile(suffix=suffix, delete=False) as tmp:
        tmp.write(await file.read())
        path = tmp.name

    try:
        audio_path = os.path.join(out_dir, "audio.wav")
        run(["ffmpeg", "-y", "-i", path, "-vn", "-ac", "1", "-ar", "16000", audio_path])

        scene_list = detect(path, ContentDetector())
        video_segments = []
        for index, (start, end) in enumerate(scene_list):
            start_ms = int(start.get_seconds() * 1000)
            end_ms = int(end.get_seconds() * 1000)
            keyframe = os.path.join(out_dir, f"scene_{index}.jpg")
            midpoint = (start.get_seconds() + end.get_seconds()) / 2
            run(["ffmpeg", "-y", "-ss", f"{midpoint:.3f}", "-i", path,
                 "-frames:v", "1", "-q:v", "3", keyframe])
            video_segments.append(
                {
                    "segment_id": f"vs_{uuid.uuid4().hex[:12]}",
                    "scene_index": index,
                    "start_ms": start_ms,
                    "end_ms": end_ms,
                    "keyframe_shared_path": keyframe,
                }
            )
        return {
            "provider_id": "video",
            "artifact_id": artifact_id,
            "job_id": job_id,
            "audio_shared_path": audio_path,
            "video_segments": video_segments,
        }
    finally:
        os.unlink(path)
