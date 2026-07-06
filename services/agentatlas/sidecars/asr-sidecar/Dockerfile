# ASR sidecar: faster-whisper (MIT) behind the Parser Provider HTTP surface.
FROM python:3.11-slim

ARG PIP_INDEX_URL=https://pypi.org/simple

RUN pip install --no-cache-dir -i ${PIP_INDEX_URL} \
    "fastapi>=0.115" "uvicorn[standard]>=0.30" python-multipart "faster-whisper>=1.0"

WORKDIR /app
COPY app.py .

EXPOSE 5003
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "5003"]
