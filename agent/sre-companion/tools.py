"""Workflow tool client for invoking Cloud Workflows."""

import json
import logging
import time

from google.api_core.exceptions import GoogleAPICallError
from google.cloud.workflows.executions_v1 import ExecutionsClient
from google.cloud.workflows.executions_v1.types import (
    CreateExecutionRequest,
    Execution,
    GetExecutionRequest,
)

logger = logging.getLogger("sre-companion.tools")

WORKFLOW_TIMEOUT_SECONDS = 120
POLL_INTERVAL_SECONDS = 2
POLL_MAX_INTERVAL_SECONDS = 10


class WorkflowToolClient:
    """Client for invoking Cloud Workflows as agent tools."""

    def __init__(self, project_id, region, client=None):
        self.project_id = project_id
        self.region = region
        self._client = client or ExecutionsClient()

    def invoke(self, workflow_name, parameters):
        """Invoke a Cloud Workflow and return the result."""
        parent = (
            f"projects/{self.project_id}/locations/{self.region}"
            f"/workflows/{workflow_name}"
        )

        execution = Execution(argument=json.dumps(parameters))

        try:
            logger.info("Invoking workflow %s with params: %s", workflow_name, parameters)
            result = self._client.create_execution(
                request=CreateExecutionRequest(parent=parent, execution=execution)
            )
            execution_name = result.name
        except GoogleAPICallError as e:
            logger.exception("Failed to create execution for %s", workflow_name)
            return {"error": f"Failed to invoke workflow {workflow_name}: {e}"}

        start_time = time.monotonic()
        interval = POLL_INTERVAL_SECONDS
        while time.monotonic() - start_time < WORKFLOW_TIMEOUT_SECONDS:
            try:
                execution_result = self._client.get_execution(
                    request=GetExecutionRequest(name=execution_name)
                )
            except GoogleAPICallError as e:
                logger.exception("Failed to poll execution %s", execution_name)
                return {"error": f"Failed to poll workflow execution: {e}"}

            state = execution_result.state

            if state == Execution.State.SUCCEEDED:
                try:
                    return json.loads(execution_result.result)
                except (json.JSONDecodeError, TypeError):
                    return {"result": execution_result.result}

            if state == Execution.State.FAILED:
                error_msg = execution_result.error.message if execution_result.error else "Unknown error"
                logger.error("Workflow %s failed: %s", workflow_name, error_msg)
                return {"error": f"Workflow {workflow_name} failed: {error_msg}"}

            if state == Execution.State.CANCELLED:
                return {"error": f"Workflow {workflow_name} was cancelled"}

            time.sleep(interval)
            interval = min(interval * 1.5, POLL_MAX_INTERVAL_SECONDS)

        return {"error": f"Workflow {workflow_name} timed out after {WORKFLOW_TIMEOUT_SECONDS}s"}
