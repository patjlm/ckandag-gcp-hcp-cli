"""Core SRE companion agent with multi-turn reasoning loop."""

import json
import logging
import uuid

from vertexai.generative_models import (
    Content,
    FunctionDeclaration,
    GenerationConfig,
    GenerativeModel,
    Part,
    Tool,
)

from prompts import (
    _clean_parameters,
    _load_tools_config,
    build_function_declarations,
    build_system_prompt,
)

logger = logging.getLogger("sre-companion.agent")

MAX_REMOTE_TOOL_ITERATIONS = 15
MAX_INPUT_TOKENS = 1_000_000  # leave headroom below the 1,048,576 hard limit


def run_agent(history, client_tools, tool_result=None,
              workflow_tool_client=None, model_name="gemini-2.5-flash",
              max_iterations=None):
    """Run a single conversational turn of the SRE companion agent.

    This is a generator that yields NDJSON events.

    Args:
        history: Full conversation history as [{role, content}, ...].
        client_tools: Client-side tool definitions [{name, description, parameters}, ...].
        tool_result: Optional result from a previous client-side tool call.
        workflow_tool_client: WorkflowToolClient for remote tool execution.
        model_name: Vertex AI model name.

    Yields:
        Event dicts for NDJSON streaming.
    """
    # Load remote tools config once for the entire turn
    try:
        tools_config = _load_tools_config()
    except FileNotFoundError:
        tools_config = {}

    client_tool_names = {t["name"] for t in client_tools}

    # Build Gemini tools and system prompt, reusing the single config load
    remote_declarations = build_function_declarations(tools_config)
    client_declarations = _build_client_declarations(client_tools)
    all_declarations = remote_declarations + client_declarations

    system_prompt = build_system_prompt(tools_config=tools_config, client_tools=client_tools)

    model = GenerativeModel(
        model_name=model_name,
        system_instruction=system_prompt,
        tools=[Tool(function_declarations=all_declarations)] if all_declarations else None,
        generation_config=GenerationConfig(temperature=0.1, max_output_tokens=65535),
    )

    # Convert history to Gemini Content objects, trimming if too long
    gemini_history = _convert_history(history[:-1]) if len(history) > 1 else []
    gemini_history = _trim_history(model, gemini_history, MAX_INPUT_TOKENS)
    chat = model.start_chat(history=gemini_history)

    # Determine what to send as the next message
    if tool_result is not None:
        tool_name = tool_result.get("tool", "")
        tool_params = tool_result.get("parameters", {})
        if tool_result.get("error"):
            func_response = {"status": "error", "error": tool_result["error"]}
        else:
            func_response = tool_result.get("result", {})
            if isinstance(func_response, str):
                try:
                    func_response = json.loads(func_response)
                except (json.JSONDecodeError, TypeError):
                    func_response = {"result": func_response}

        # Inject the assistant's function_call into chat history so the model
        # knows it already called this tool and doesn't re-issue the call.
        chat.history.append(
            Content(role="model", parts=[Part.from_function_call(
                name=tool_name, args=tool_params,
            )])
        )

        next_message = Content(
            parts=[Part.from_function_response(name=tool_name, response=func_response)]
        )
    else:
        last_message = history[-1]["content"] if history else ""
        next_message = last_message

    try:
        response = chat.send_message(next_message)
    except Exception as e:
        logger.exception("Failed to send message: %s", e)
        yield {"event": "error", "error": f"Failed to communicate with AI model: {e}"}
        return

    # Iterative tool-calling loop
    limit = max_iterations if max_iterations and max_iterations > 0 else MAX_REMOTE_TOOL_ITERATIONS
    iteration = 0
    while iteration < limit:
        iteration += 1

        function_calls = _extract_function_calls(response)
        text = _extract_text(response)

        if not function_calls:
            if text:
                yield {"event": "text", "content": text}
            yield {"event": "done"}
            return

        # Emit any text that accompanies the tool calls
        if text:
            yield {"event": "text", "content": text}

        # Check if any call is a client-side tool (only one client tool per turn)
        for fc in function_calls:
            if fc.name in client_tool_names:
                call_id = str(uuid.uuid4())
                yield {
                    "event": "tool_call",
                    "call_id": call_id,
                    "tool": fc.name,
                    "parameters": dict(fc.args) if fc.args else {},
                }
                return

        # All calls are remote — execute them and collect responses
        response_parts = []
        for fc in function_calls:
            tool_name = fc.name
            tool_args = dict(fc.args) if fc.args else {}

            workflow_name = tools_config.get(tool_name, {}).get("workflow")
            if not workflow_name:
                result = {"error": f"Unknown remote tool: {tool_name}"}
            else:
                result = workflow_tool_client.invoke(workflow_name, tool_args)

            yield {
                "event": "tool_result",
                "tool": tool_name,
                "result": _summarize_result(result),
            }

            response_parts.append(
                Part.from_function_response(name=tool_name, response=result)
            )

        try:
            response = chat.send_message(Content(parts=response_parts))
        except Exception as e:
            logger.exception("Failed to send tool results: %s", e)
            yield {"event": "error", "error": str(e)}
            return

    yield {
        "event": "text",
        "content": "I've reached the maximum number of tool calls for this turn. "
                   "Please provide more guidance or ask a more specific question.",
    }
    yield {"event": "done"}


def _trim_history(model, gemini_history, max_tokens):
    """Drop oldest messages from history if token count exceeds the limit.

    Keeps removing pairs from the front (to maintain user/model alternation)
    until the token count is within budget. Returns the trimmed history.
    """
    if not gemini_history:
        return gemini_history

    try:
        count = model.count_tokens(gemini_history).total_tokens
    except Exception:
        return gemini_history  # best-effort; skip trimming if count fails

    while count > max_tokens and len(gemini_history) > 2:
        # Drop the two oldest messages (user + model pair)
        gemini_history = gemini_history[2:]
        logger.info("Trimming history: %d tokens > %d limit, %d messages remaining",
                     count, max_tokens, len(gemini_history))
        try:
            count = model.count_tokens(gemini_history).total_tokens
        except Exception:
            break

    return gemini_history


def _convert_history(messages):
    """Convert chat history to Gemini Content objects."""
    contents = []
    for msg in messages:
        role = "model" if msg["role"] == "assistant" else "user"
        contents.append(
            Content(role=role, parts=[Part.from_text(msg["content"])])
        )
    return contents


def _extract_function_calls(response):
    """Extract all function calls from a Gemini response."""
    if not response.candidates:
        return []
    return [
        part.function_call
        for part in response.candidates[0].content.parts
        if part.function_call and part.function_call.name
    ]


def _extract_text(response):
    """Extract text content from a Gemini response."""
    if not response.candidates:
        return ""
    parts = response.candidates[0].content.parts
    texts = [part.text for part in parts if part.text]
    return "\n".join(texts)


def _build_client_declarations(client_tools):
    """Convert client-side tool definitions to Gemini FunctionDeclaration objects."""
    return [
        FunctionDeclaration(
            name=t["name"],
            description=t["description"],
            parameters=_clean_parameters(t.get("parameters", {})),
        )
        for t in client_tools
    ]


def _summarize_result(result):
    """Create a brief summary of a tool result."""
    if isinstance(result, dict):
        if "error" in result:
            return f"Error: {result['error'][:200]}"
        kind = result.get("kind", "")
        items = result.get("items", [])
        if items:
            return f"{kind}: {len(items)} items" if kind else f"{len(items)} items"
        name = result.get("metadata", {}).get("name", "")
        if name:
            return f"{kind}: {name}"
        return json.dumps(result)[:200]
    return str(result)[:200]
