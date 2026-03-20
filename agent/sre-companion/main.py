"""Flask HTTP service for the SRE companion agent."""

import json
import logging
import os
import sys

from flask import Flask, Response, jsonify, request, stream_with_context

import vertexai

from agent import run_agent
from tools import WorkflowToolClient

app = Flask(__name__)

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    stream=sys.stderr,
)
logger = logging.getLogger("sre-companion")

# Initialize once at startup
_PROJECT_ID = os.environ.get("GCP_PROJECT_ID", "")
_REGION = os.environ.get("GCP_REGION", "")
_MODEL = os.environ.get("VERTEX_AI_MODEL", "gemini-2.5-flash")

if _PROJECT_ID and _REGION:
    vertexai.init(project=_PROJECT_ID, location=_REGION)
    _WORKFLOW_CLIENT = WorkflowToolClient(project_id=_PROJECT_ID, region=_REGION)
else:
    _WORKFLOW_CLIENT = None


@app.route("/health", methods=["GET"])
def health():
    return jsonify({"status": "healthy"})


@app.route("/chat", methods=["POST"])
def chat():
    """Handle a conversational turn with the SRE companion agent."""
    if not _PROJECT_ID or not _REGION:
        return jsonify({"error": "GCP_PROJECT_ID and GCP_REGION environment variables must be set"}), 500

    data = request.get_json(silent=True)
    if not data:
        return jsonify({"error": "Request body must be JSON"}), 400

    history = data.get("history", [])
    client_tools = data.get("tools", [])
    tool_result = data.get("tool_result")
    max_iterations = data.get("max_iterations")

    if not history and tool_result is None:
        return jsonify({"error": "history or tool_result is required"}), 400

    def stream():
        try:
            for event in run_agent(
                history=history,
                client_tools=client_tools,
                tool_result=tool_result,
                workflow_tool_client=_WORKFLOW_CLIENT,
                model_name=_MODEL,
                max_iterations=max_iterations,
            ):
                yield json.dumps(event) + "\n"
        except Exception as e:
            logger.exception("Agent error: %s", e)
            yield json.dumps({"event": "error", "error": str(e)}) + "\n"

    return Response(
        stream_with_context(stream()),
        mimetype="application/x-ndjson",
    )


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8080, debug=False)
