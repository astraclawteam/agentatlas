# MinerU sidecar built from the official PyPI package (AGPL-3.0, isolated as
# a separate process invoked over HTTP; see the third-party license matrix).
FROM python:3.11-slim

ARG PIP_INDEX_URL=https://pypi.org/simple

RUN apt-get update \
    && apt-get install -y --no-install-recommends libgl1 libglib2.0-0 \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir -i ${PIP_INDEX_URL} -U "mineru[core]" uvicorn

EXPOSE 8000
CMD ["mineru-api", "--host", "0.0.0.0", "--port", "8000"]
