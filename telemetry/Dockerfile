FROM python:3.11-slim

LABEL org.opencontainers.image.title="llmtelemetry" \
      org.opencontainers.image.description="LLM-MCP telemetry service" \
      org.opencontainers.image.source="https://github.com/plagness/LLM-MCP.git" \
      ns.module="llm" \
      ns.component="telemetry"

ENV PYTHONUNBUFFERED=1
WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends git \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt

COPY llm_telemetry /app/llm_telemetry

CMD ["python", "-m", "llm_telemetry.main"]
