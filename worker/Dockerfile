FROM python:3.11-slim
LABEL org.opencontainers.image.title="llmworker" \
      org.opencontainers.image.description="LLM-MCP worker service" \
      org.opencontainers.image.source="https://github.com/plagness/LLM-MCP.git" \
      ns.module="llm" \
      ns.component="worker"
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY llm_worker ./llm_worker
CMD ["python", "-m", "llm_worker.main"]
