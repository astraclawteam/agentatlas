# Video sidecar: ffmpeg + PySceneDetect behind the Parser Provider surface.
FROM python:3.11-slim

ARG PIP_INDEX_URL=https://pypi.org/simple

RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir -i ${PIP_INDEX_URL} \
    "fastapi>=0.115" "uvicorn[standard]>=0.30" python-multipart \
    "scenedetect[opencv-headless]>=0.6"

WORKDIR /app
COPY app.py .

EXPOSE 5004
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "5004"]
