FROM python:3.12-slim

# Security: run as non-root
RUN groupadd -r agent && useradd -r -g agent -d /app -s /sbin/nologin agent

WORKDIR /app

# Install dependencies first for layer caching
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy application code
COPY --chown=agent:agent *.py ./

# Copy build-time dependencies (staged before docker build)
COPY --chown=agent:agent agent_tools.yaml ./
COPY --chown=agent:agent knowledge/ ./knowledge/

USER agent

EXPOSE 8080

CMD ["gunicorn", "--bind", "0.0.0.0:8080", "--workers", "1", "--threads", "4", "--timeout", "300", "main:app"]
