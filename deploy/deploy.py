#!/usr/bin/env python3
"""Create/update authenticated SCF WebSocket nodes; never print credentials."""
import argparse
import base64
import io
import json
import os
from pathlib import Path
import re
import secrets
import sys
import time
import tomllib
from urllib.parse import urlsplit
import zipfile

MARKER = "scf-tunnel-proxy managed:v1"
REGIONS = ["ap-shanghai", "ap-guangzhou", "ap-beijing", "ap-chengdu", "ap-nanjing"]


def private_write(path, text):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = path.with_name(path.name + ".tmp")
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as output:
            output.write(text)
    except BaseException:
        temporary.unlink(missing_ok=True)
        raise
    os.replace(temporary, path)
    path.chmod(0o600)


def resolve_path(base, value):
    path = Path(value).expanduser()
    return path if path.is_absolute() else (base / path).resolve()


def load_settings(config_path):
    config_path = Path(config_path).resolve()
    with config_path.open("rb") as source:
        config = tomllib.load(source)
    deployment = config.setdefault("deployment", {})
    deployment.setdefault("function_name", "scf_tunnel_proxy")
    deployment.setdefault("namespace", "default")
    deployment.setdefault("regions", REGIONS.copy())
    if not re.fullmatch(r"[A-Za-z][A-Za-z0-9_-]{0,59}", deployment["function_name"]):
        raise ValueError("invalid function_name")
    regions = deployment["regions"]
    if not regions or len(set(regions)) != len(regions):
        raise ValueError("regions must be nonempty and unique")
    if any(not re.fullmatch(r"[a-z]+-[a-z0-9-]+", region) for region in regions):
        raise ValueError("invalid region identifier")
    output = config.get("output", {})
    state = resolve_path(config_path.parent, output.get("state_dir", "../runtime/deploy"))
    agent = resolve_path(config_path.parent, output.get("agent_config", "../runtime/agent.json"))
    return config, state, agent


def package_binary(binary):
    binary = Path(binary)
    data = binary.read_bytes()
    # The documented SCF package is a statically linked Linux x86-64 executable.
    if len(data) < 20 or data[:4] != b"\x7fELF" or data[4] != 2 or int.from_bytes(data[18:20], "little") != 62:
        raise ValueError("binary must be a Linux amd64 ELF executable; use ops/build.sh")
    stream = io.BytesIO()
    with zipfile.ZipFile(stream, "w", zipfile.ZIP_DEFLATED) as archive:
        for name, content in [
            ("scf-tunnel", data),
            ("scf_bootstrap", b"#!/bin/sh\ncd /var/user\nexec ./scf-tunnel -node\n"),
        ]:
            info = zipfile.ZipInfo(name)
            info.external_attr = 0o100755 << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            archive.writestr(info, content)
    return stream.getvalue()


def wait_active(client, models, function, namespace):
    request = models.GetFunctionRequest()
    request.FunctionName, request.Namespace = function, namespace
    for _ in range(90):
        state = client.GetFunction(request)
        if state.Status == "Active":
            return state
        if state.Status in ("CreateFailed", "UpdateFailed"):
            raise RuntimeError("function entered " + state.Status)
        time.sleep(2)
    raise TimeoutError("function activation timed out")


def function_url(client, models, name, namespace):
    request = models.ListTriggersRequest()
    request.FunctionName, request.Namespace = name, namespace
    triggers = client.ListTriggers(request).Triggers or []
    if not any(trigger.Type == "http" for trigger in triggers):
        create = models.CreateTriggerRequest()
        create.FunctionName, create.Namespace = name, namespace
        create.Type, create.TriggerName, create.Enable = "http", "tunnel_url", "OPEN"
        create.TriggerDesc = json.dumps({"AuthType": "NONE", "NetConfig": {
            "EnableIntranet": False, "EnableExtranet": True}})
        client.CreateTrigger(create)
    for _ in range(30):
        for trigger in client.ListTriggers(request).Triggers or []:
            if trigger.Type != "http":
                continue
            desc = json.loads(trigger.TriggerDesc)
            net = desc.get("NetConfig", {})
            value = net.get("WssExtranetUrl") or net.get("ExtranetUrl", "").replace("https://", "wss://", 1)
            if value:
                parsed = urlsplit(value)
                if parsed.scheme != "wss" or not parsed.hostname:
                    raise ValueError("function returned invalid WSS URL")
                return value.rstrip("/") + "/tunnel"
        time.sleep(2)
    raise TimeoutError("function URL was not assigned")


def deploy_region(client, models, sdk_error, settings, encoded_code, token, update):
    name, namespace = settings["function_name"], settings["namespace"]
    request = models.GetFunctionRequest()
    request.FunctionName, request.Namespace = name, namespace
    try:
        existing = client.GetFunction(request)
    except sdk_error as error:
        if "ResourceNotFound" not in error.get_code():
            raise
        existing = None
    if existing:
        if existing.Description != MARKER:
            raise RuntimeError("function name is owned by another deployment; choose a new name")
        variables = getattr(getattr(existing, "Environment", None), "Variables", None) or []
        current_token = next((v.Value for v in variables if v.Key == "CLOUD_PROXY_TOKEN"), None)
        if current_token != token:
            raise RuntimeError("stored node token does not match cloud; restore state or use a new function name")
        if update:
            request = models.UpdateFunctionCodeRequest()
            request.FunctionName, request.Namespace, request.ZipFile = name, namespace, encoded_code
            client.UpdateFunctionCode(request)
            time.sleep(2)
    else:
        request = models.CreateFunctionRequest()
        request.from_json_string(json.dumps({
            "FunctionName": name, "Namespace": namespace, "Runtime": "Python3.9", "Type": "HTTP",
            "ProtocolType": "WS", "ProtocolParams": {"WSParams": {"IdleTimeOut": 120}},
            "Timeout": 7200, "MemorySize": 128, "InitTimeout": 30, "Description": MARKER,
            "Code": {"ZipFile": encoded_code},
            "Environment": {"Variables": [{"Key": "CLOUD_PROXY_TOKEN", "Value": token}]},
            "PublicNetConfig": {"PublicNetStatus": "ENABLE", "EipConfig": {"EipStatus": "DISABLE"}},
            "AutoCreateClsTopic": "FALSE", "IgnoreSysLog": True,
        }))
        client.CreateFunction(request)
    state = wait_active(client, models, name, namespace)
    if state.MemorySize != 128 or state.Timeout != 7200:
        raise RuntimeError("cloud memory/timeout differs from the budget model; inspect configuration")
    return function_url(client, models, name, namespace)


def make_agent_config(config, nodes, token, existing=None):
    if existing:
        # Preserve endpoint authentication and the original ledger during upgrades.
        result = dict(existing)
        if result.get("token") != token:
            raise ValueError("existing agent uses a different token; refusing overwrite")
    else:
        supplied = config.get("agent", {})
        result = {
            "http_addr": supplied.get("http_addr", "0.0.0.0:10800"),
            "socks_addr": supplied.get("socks_addr", "0.0.0.0:10801"),
            "dashboard_addr": supplied.get("dashboard_addr", "127.0.0.1:8081"),
            "user": supplied.get("user", "cloudproxy"), "password": secrets.token_urlsafe(24), "token": token,
            "ledger_path": supplied.get("ledger_path", "/var/lib/scf-tunnel-proxy/usage.json"),
            "monthly_limit_yuan": supplied.get("monthly_limit_yuan", 8.0),
            "max_connections": supplied.get("max_connections", 32),
        }
    result["nodes"] = [{"region": n["region"], "url": n["url"]} for n in nodes]
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", default="config/deploy.toml")
    parser.add_argument("--binary", default="dist/scf-tunnel-linux-amd64")
    parser.add_argument("--update", action="store_true", help="update code of this tool's existing managed nodes")
    parser.add_argument("--dry-run", action="store_true", help="validate and package; no cloud calls or token files")
    args = parser.parse_args(argv)
    config, state_dir, agent_path = load_settings(args.config)
    package = package_binary(args.binary)
    settings = config["deployment"]
    if args.dry_run:
        print(json.dumps({"function": settings["function_name"], "regions": settings["regions"],
                          "package_bytes": len(package), "memory_mb": 128, "timeout_seconds": 7200}, indent=2))
        return 0
    from tencentcloud.common import credential
    from tencentcloud.common.exception.tencent_cloud_sdk_exception import TencentCloudSDKException
    from tencentcloud.common.profile.client_profile import ClientProfile
    from tencentcloud.common.profile.http_profile import HttpProfile
    from tencentcloud.scf.v20180416 import scf_client, models
    credentials = config.get("tencent", {})
    secret_id = os.environ.get("TENCENTCLOUD_SECRET_ID") or credentials.get("secret_id")
    secret_key = os.environ.get("TENCENTCLOUD_SECRET_KEY") or credentials.get("secret_key")
    session = os.environ.get("TENCENTCLOUD_SESSION_TOKEN") or credentials.get("session_token")
    if not secret_id or not secret_key or "YOUR_" in secret_id or "YOUR_" in secret_key:
        raise ValueError("provide Tencent credentials in the environment or private config file")
    token_path = state_dir / "node-token"
    token = token_path.read_text().strip() if token_path.exists() else secrets.token_urlsafe(40)
    if len(token) < 24:
        raise ValueError("stored node token is unexpectedly short")
    private_write(token_path, token)
    current_agent = json.loads(agent_path.read_text()) if agent_path.exists() else None
    if current_agent and current_agent.get("token") != token:
        raise ValueError("agent and deployment state use different tokens; restore matching files")
    cred = credential.Credential(secret_id, secret_key, session)
    http_profile = HttpProfile()
    http_profile.endpoint, http_profile.reqTimeout = "scf.tencentcloudapi.com", 120
    client_profile = ClientProfile()
    client_profile.httpProfile = http_profile
    results, failures = [], []
    for region in settings["regions"]:
        try:
            client = scf_client.ScfClient(cred, region, client_profile)
            url = deploy_region(client, models, TencentCloudSDKException, settings,
                                base64.b64encode(package).decode(), token, args.update)
            results.append({"region": region, "url": url})
            print(region, "READY", flush=True)
        except Exception as error:
            # SDK errors may include details; redact all known credentials before reporting.
            message = str(error)
            for sensitive in (secret_id, secret_key, session, token):
                if sensitive:
                    message = message.replace(sensitive, "[REDACTED]")
            failures.append({"region": region, "error": message[:700]})
            print(region, "FAILED", message[:700], flush=True)
    private_write(state_dir / "deployment-results.json", json.dumps({"nodes": results, "failures": failures}, indent=2))
    if failures:
        print("Partial deployment: agent config was not changed. Fix errors and rerun; created functions remain.", file=sys.stderr)
        return 1
    agent = make_agent_config(config, results, token, current_agent)
    private_write(agent_path, json.dumps(agent, indent=2))
    print("Agent configuration written to", agent_path, "(600); credentials are inside this private file.")
    print("This confirms provisioning only. Test authenticated TCP/TLS forwarding before production use.")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:
        print(type(error).__name__ + ": " + str(error), file=sys.stderr)
        sys.exit(1)
