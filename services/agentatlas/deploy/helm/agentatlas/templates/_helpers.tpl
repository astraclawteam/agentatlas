{{- define "agentatlas.image" -}}
{{- $registry := .root.Values.image.registry -}}
{{- if $registry -}}
{{ printf "%s/%s/%s:%s" $registry .root.Values.image.repositoryPrefix .service .root.Values.image.tag }}
{{- else -}}
{{ printf "%s/%s:%s" .root.Values.image.repositoryPrefix .service .root.Values.image.tag }}
{{- end -}}
{{- end -}}

{{- define "agentatlas.env" -}}
- name: ATLAS_POSTGRES_DSN
  value: {{ .Values.config.postgresDsn | quote }}
- name: ATLAS_OPENSEARCH_ADDRESSES
  value: {{ .Values.config.opensearchAddresses | quote }}
- name: ATLAS_NATS_URL
  value: {{ .Values.config.natsUrl | quote }}
- name: ATLAS_OBJECT_ENDPOINT
  value: {{ .Values.config.objectEndpoint | quote }}
- name: ATLAS_OBJECT_BUCKET
  value: {{ .Values.config.objectBucket | quote }}
- name: ATLAS_OBJECT_ACCESS_KEY
  value: {{ .Values.config.objectAccessKey | quote }}
- name: ATLAS_OBJECT_SECRET_KEY
  value: {{ .Values.config.objectSecretKey | quote }}
- name: ATLAS_LLMROUTER_BASE_URL
  value: {{ .Values.config.llmrouterBaseUrl | quote }}
- name: ATLAS_LLMROUTER_API_KEY
  value: {{ .Values.config.llmrouterApiKey | quote }}
- name: ATLAS_NEXUS_BASE_URL
  value: {{ .Values.config.nexusBaseUrl | quote }}
- name: ATLAS_NEXUS_CLIENT_ID
  value: {{ .Values.config.nexusClientId | quote }}
- name: ATLAS_NEXUS_SERVICE_SECRET_FILE
  value: /var/run/secrets/agentnexus/client-secret
- name: ATLAS_NEXUS_APPROVAL_FACTS_SECRET_FILE
  value: /var/run/secrets/agentnexus/approval-facts-secret
- name: ATLAS_NEXUS_BROWSER_CLIENT_ID
  value: {{ .Values.config.nexusClientId | quote }}
- name: ATLAS_NEXUS_BROWSER_CLIENT_SECRET_FILE
  value: /var/run/secrets/agentnexus/browser-client-secret
- name: ATLAS_BROWSER_SESSION_ENCRYPTION_KEY_FILE
  value: /var/run/secrets/agentnexus/browser-session-key
- name: ATLAS_PUBLIC_URL
  value: {{ .Values.config.atlasPublicUrl | quote }}
- name: ATLAS_DOCLING_URL
  value: {{ .Values.config.doclingUrl | quote }}
- name: ATLAS_MINERU_URL
  value: {{ .Values.config.mineruUrl | quote }}
- name: ATLAS_ASR_URL
  value: {{ .Values.config.asrUrl | quote }}
- name: ATLAS_VIDEO_URL
  value: {{ .Values.config.videoUrl | quote }}
{{- end -}}

{{- define "agentatlas.nexusSecretVolumeMount" -}}
- name: agentnexus-service-secret
  mountPath: /var/run/secrets/agentnexus
  readOnly: true
{{- end -}}

{{- define "agentatlas.nexusSecretVolume" -}}
- name: agentnexus-service-secret
  secret:
    secretName: {{ .Values.config.nexusServiceSecretName | quote }}
    items:
      - key: {{ .Values.config.nexusServiceSecretKey | quote }}
        path: client-secret
        mode: 0400
      - key: {{ .Values.config.nexusApprovalFactsSecretKey | quote }}
        path: approval-facts-secret
        mode: 0400
      - key: {{ .Values.config.nexusBrowserClientSecretKey | quote }}
        path: browser-client-secret
        mode: 0400
      - key: {{ .Values.config.browserSessionEncryptionKey | quote }}
        path: browser-session-key
        mode: 0400
{{- end -}}
