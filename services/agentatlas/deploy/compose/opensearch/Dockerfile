# OpenSearch with the official analysis-smartcn plugin preinstalled —
# the Chinese tokenization baseline for summary_text / sanitized_snippet.
ARG OPENSEARCH_VERSION=3.7.0
FROM opensearchproject/opensearch:${OPENSEARCH_VERSION}

RUN /usr/share/opensearch/bin/opensearch-plugin install --batch analysis-smartcn

COPY opensearch.yml /usr/share/opensearch/config/opensearch.yml
