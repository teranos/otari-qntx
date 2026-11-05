"""otari inside pyre: otari's ASGI app on an asyncio loop of its own, in the
plugin's process, asked through httpx's ASGI transport. Nothing listens.

The handler pyre runs (handlers/gateway.py) calls chat, completions and usage
with the request and the plugin's config. The first call starts otari: its
lifespan (checks, migrations, the declared budgets and key) runs once, and the
loop then lives as long as the plugin. A call blocks on a future of that loop,
which lets the GIL go while otari works (teranos/pyre, "A loop started once
serves the handlers that wait on it").
"""

import asyncio
import concurrent.futures
import json
import os
import secrets
import sys
import threading
from pathlib import Path
from urllib.parse import parse_qs

HERE = Path(__file__).parent

# otari's routes, below its API root.
CHAT = "/api/v1/chat/completions"
USAGE = "/api/v1/usage/requests/"

# The headers otari says how it served a call with, and the ones the gateway
# hands back beside them.
PASSED_BACK = ("content-type", "retry-after")

_started = threading.Lock()
_otari = None


class _Otari:
    def __init__(self, loop, client, service_key, master_key, timeout):
        self.loop = loop
        self.client = client
        self.service_key = service_key
        self.master_key = master_key
        self.timeout = timeout

    def ask(self, method, path, key, body=None, headers=None):
        """otari's answer, or TimeoutError once timeout_seconds pass; the call
        is then cancelled at otari, so it stops spending upstream."""
        sent = {"Authorization": "Bearer " + key}
        sent.update(headers or {})
        coro = self.client.request(method, path, content=body, headers=sent)
        future = asyncio.run_coroutine_threadsafe(coro, self.loop)
        try:
            return future.result(self.timeout)
        except concurrent.futures.TimeoutError:
            future.cancel()
            raise TimeoutError("otari did not answer within %s s" % self.timeout) from None


def _kept(path, make):
    """A secret otaripy made once and keeps, readable by the plugin's user alone."""
    if not path.exists():
        path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        try:
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        except FileExistsError:
            pass
        else:
            with os.fdopen(fd, "w") as f:
                f.write(make())
    return path.read_text().strip()


def otari(config):
    """otari, started on first use."""
    global _otari
    with _started:
        if _otari is not None:
            return _otari

        upstream = config.get("openrouter_api_key", "")
        if not upstream:
            raise RuntimeError(
                "the plugin's config names no openrouter_api_key; set it to "
                "ssm:///q/box/openrouter-qntx/api-key and the node hands the plugin its value"
            )
        data = Path(config.get("data_dir") or "/var/lib/qntx/otaripy")
        master = _kept(data / "master-key", lambda: secrets.token_urlsafe(48))
        service = _kept(data / "service-key", lambda: "tk-" + secrets.token_urlsafe(48))
        timeout = float(config.get("timeout_seconds") or 300)

        os.environ.update(
            {
                "OPENROUTER_API_KEY": upstream,
                "OTARI_QNTX_KEY": service,
                "OTARI_MASTER_KEY": master,
                "OTARI_MIGRATIONS_DIR": str(HERE / "alembic"),
                "OTARI_ALEMBIC_INI": str(HERE / "alembic.ini"),
            }
        )

        # pyre captures a handler's output by swapping sys.stdout and
        # sys.stderr; otari's loggers are made here and outlive this call, so
        # they are made on the process's own streams.
        captured = sys.stdout, sys.stderr
        sys.stdout, sys.stderr = sys.__stdout__, sys.__stderr__
        try:
            import httpx
            from gateway.core.config import load_config
            from gateway.main import create_app

            # otari_config names another config.yml: the check in the flake
            # points otari at a stand-in upstream with one.
            gateway_config = load_config(config.get("otari_config") or str(HERE / "config.yml"))
            gateway_config.database_url = "sqlite:///" + str(data / "otari.db")
            gateway_config.master_key = master
            app = create_app(gateway_config)
        finally:
            sys.stdout, sys.stderr = captured

        loop = asyncio.new_event_loop()
        ready = threading.Event()
        held = {}

        async def serve():
            try:
                async with app.router.lifespan_context(app):
                    held["client"] = httpx.AsyncClient(
                        transport=httpx.ASGITransport(app=app),
                        base_url="http://otari",
                        timeout=timeout,
                    )
                    ready.set()
                    await asyncio.Event().wait()
            except BaseException:
                import traceback

                held["failed"] = traceback.format_exc()
                ready.set()
                raise

        def run():
            asyncio.set_event_loop(loop)
            loop.run_until_complete(serve())

        threading.Thread(target=run, name="otaripy-otari", daemon=True).start()
        if not ready.wait(timeout=600):
            raise RuntimeError("otari did not finish starting within 600 s")
        if "failed" in held:
            raise RuntimeError("otari did not start:\n" + held["failed"])

        _otari = _Otari(loop, held["client"], service, master, timeout)
        return _otari


# --- LLMService ---------------------------------------------------------------

# otari's status, as the gRPC code the node tells apart: retry later from fix
# the request from fix the deployment. As the Go plugin's statusFrom has it.
def _code(status):
    if status in (400, 413, 422):
        return "INVALID_ARGUMENT"
    if status == 401:
        return "UNAUTHENTICATED"
    if status == 403:
        return "PERMISSION_DENIED"
    if status in (402, 424):
        return "FAILED_PRECONDITION"
    if status == 404:
        return "NOT_FOUND"
    if status in (408, 504):
        return "DEADLINE_EXCEEDED"
    if status == 429:
        return "RESOURCE_EXHAUSTED"
    if status >= 500:
        return "UNAVAILABLE"
    return "UNKNOWN"


def _refusal(response):
    try:
        said = response.json()
    except ValueError:
        said = {}
    detail = said.get("detail") if isinstance(said, dict) else None
    if detail is None and isinstance(said, dict) and isinstance(said.get("error"), dict):
        detail = said["error"].get("message")
    code = response.headers.get("otari-error-code", "")
    words = "otari %d%s: %s" % (response.status_code, " " + code if code else "", detail or response.text)
    request_id = response.headers.get("otari-request-id", "")
    if request_id:
        words += " [Otari-Request-ID %s]" % request_id
    return {"error": {"code": _code(response.status_code), "message": words}}


def _messages(request):
    """The call's messages: messages first, else the deprecated prompts."""
    messages = [{"role": m["role"], "content": m["content"]} for m in request.get("messages") or []]
    if not messages:
        if request.get("system_prompt"):
            messages.append({"role": "system", "content": request["system_prompt"]})
        if request.get("user_prompt"):
            messages.append({"role": "user", "content": request["user_prompt"]})
    attachments = [a for a in request.get("attachments") or [] if a.get("data")]
    if attachments:
        users = [i for i, m in enumerate(messages) if m["role"] == "user"]
        if not users:
            raise ValueError("the call carries attachments and no user message to carry them")
        last = messages[users[-1]]
        parts = [{"type": "text", "text": last["content"]}]
        for a in attachments:
            data = a["data"]
            if "image" in a.get("mime_type", "") or data.startswith("data:image"):
                parts.append({"type": "image_url", "image_url": {"url": data}})
                continue
            if not data.startswith("data:"):
                raise ValueError("attachment %r is not a data URI" % a.get("filename", ""))
            part = {"file_data": data}
            if a.get("filename"):
                part["filename"] = a["filename"]
            parts.append({"type": "file", "file": part})
        last["content"] = parts
    return messages


def chat(request, config):
    """The node's LLM call, as otari serves it, with what otari reported of how."""
    model = (request.get("model") or "").strip()
    if not model:
        return {"error": {"code": "INVALID_ARGUMENT", "message":
                "the call names no model: otari serves the model or routing policy a call names, and picks none itself"}}
    try:
        messages = _messages(request)
    except ValueError as e:
        return {"error": {"code": "INVALID_ARGUMENT", "message": str(e)}}
    if not messages:
        return {"error": {"code": "INVALID_ARGUMENT", "message": "the call carries no messages and no prompt"}}

    body = {"model": model, "messages": messages}
    # proto3 has no presence for these: zero is unset, and is not sent.
    if request.get("temperature"):
        body["temperature"] = request["temperature"]
    if request.get("max_tokens"):
        body["max_tokens"] = request["max_tokens"]

    o = otari(config)
    try:
        response = o.ask("POST", CHAT, o.service_key, json.dumps(body).encode(),
                         {"Content-Type": "application/json"})
    except TimeoutError as e:
        return {"error": {"code": "DEADLINE_EXCEEDED", "message": str(e)}}
    if response.status_code >= 400:
        return _refusal(response)

    said = response.json()
    h = response.headers
    usage = said.get("usage") or {}
    executed = h.get("otari-model", "")
    served = {
        "requested_provider": request.get("provider", ""),
        "requested_model": request.get("model", ""),
        "upstream_provider": h.get("otari-provider", ""),
        "executed_model": executed,
        "response_model": said.get("model", ""),
        "request_id": h.get("otari-request-id", ""),
        "pricing_source": usage.get("pricing_source", ""),
        "fallback": h.get("otari-fallback", "").lower() == "true",
    }
    if h.get("otari-attempt-count", "").isdigit():
        served["attempt_count"] = int(h["otari-attempt-count"])
    if isinstance(usage.get("cost_usd"), str):
        served["cost_usd"] = usage["cost_usd"]
    choices = said.get("choices") or []
    if not choices:
        return {"error": {"code": "UNAVAILABLE", "message":
                "otari answered with no choices [Otari-Request-ID %s]" % served["request_id"]}}
    return {
        "content": choices[0].get("message", {}).get("content") or "",
        "model": executed or said.get("model", ""),
        "prompt_tokens": usage.get("prompt_tokens", 0),
        "completion_tokens": usage.get("completion_tokens", 0),
        "total_tokens": usage.get("total_tokens", 0),
        "served": served,
    }


# --- The gateway --------------------------------------------------------------

def _refuse(status, message):
    return {"status": status, "headers": {"Content-Type": "application/json"},
            "body": json.dumps({"error": {"message": message}})}


# The end user every caller the node names no asker for is billed as: one
# identity, one per-asker budget, so a caller cannot name a fresh user per
# request and be given a fresh budget each time.
UNATTRIBUTED = "qntx-unattributed"


def _as_asker(body, headers):
    """body with its user set to who the node admitted: the asking token's DID,
    else the user, else UNATTRIBUTED. Whatever the caller put there goes."""
    asker = (headers.get("x-qntx-asker-did") or "").strip() or (headers.get("x-qntx-asker-user") or "").strip()
    try:
        asked = json.loads(body)
    except ValueError as e:
        raise ValueError("the body is not a JSON object: %s" % e) from None
    if not isinstance(asked, dict):
        raise ValueError("the body is not a JSON object")
    asked["user"] = asker or UNATTRIBUTED
    return json.dumps(asked).encode()


def completions(request, config):
    """OpenAI's chat completions, passed to otari with otaripy's key and never
    the caller's credential; who the node admitted is the body's user."""
    try:
        body = _as_asker(request["body"], request["headers"])
    except ValueError as e:
        return _refuse(400, str(e))

    o = otari(config)
    try:
        response = o.ask("POST", CHAT, o.service_key, body, {"Content-Type": "application/json"})
    except TimeoutError as e:
        return _refuse(504, str(e))
    back = {name: value for name, value in response.headers.items()
            if name.lower() in PASSED_BACK or name.lower().startswith("otari-")}
    return {"status": response.status_code, "headers": back, "body": response.content}


def usage(request, config):
    """otari's record of one call, by the Otari-Request-ID its answer carried."""
    ids = parse_qs(request.get("query", "")).get("request_id") or [""]
    request_id = ids[0].strip()
    if not request_id or "/" in request_id:
        return _refuse(400, "name the call as ?request_id=<Otari-Request-ID>")
    o = otari(config)
    try:
        response = o.ask("GET", USAGE + request_id, o.master_key)
    except TimeoutError as e:
        return _refuse(504, str(e))
    return {"status": response.status_code,
            "headers": {"Content-Type": response.headers.get("content-type", "application/json")},
            "body": response.content}
