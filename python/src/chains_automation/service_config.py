from dataclasses import dataclass
import json
import os
from pathlib import Path
from tempfile import NamedTemporaryFile
from threading import TIMEOUT_MAX
from uuid import uuid4

from .client import IDENTIFIER


def xdg(variable, fallback):
    path = Path(os.environ.get(variable, ""))
    return path if path.is_absolute() else Path.home() / fallback


@dataclass
class ServiceConfig:
    uri: str = "mqtt://localhost:1883"
    namespace: str = "chains"
    encoding: str = "json"
    authority: str = "engine"
    heartbeat_interval: int = 15
    poll_interval: float = 1.0
    service_id: str = ""
    name: str = ""
    instance: str = "default"

    @classmethod
    def load(cls, service_type, path=None, **overrides):
        instance = os.environ.get("CHAINS_INSTANCE", overrides.pop("instance", None) or "default")
        if not IDENTIFIER.fullmatch(instance) or instance in {".", ".."}:
            raise ValueError("Invalid Chains service instance")
        config = cls(instance=instance)
        explicit = os.environ.get("CHAINS_SERVICE_CONFIG", path)
        if explicit:
            paths = [Path(explicit)]
        else:
            system = os.environ.get("XDG_CONFIG_DIRS", "/etc/xdg").split(":")
            roots = [Path(root) for root in reversed(system) if Path(root).is_absolute()]
            roots.append(xdg("XDG_CONFIG_HOME", ".config"))
            paths = [root / "chains" / "services" / f"{service_type}.{instance}.json" for root in roots]
        fields = set(cls.__dataclass_fields__) - {"instance"}
        for file in paths:
            try:
                value = json.loads(file.read_text(encoding="utf-8"))
            except FileNotFoundError:
                if explicit:
                    raise
                continue
            if not isinstance(value, dict) or set(value) - fields:
                raise ValueError(f"Invalid service config fields in {file}")
            for key, item in value.items():
                setattr(config, key, item)
        for key, item in overrides.items():
            if key not in fields:
                raise TypeError(f"Unknown service option {key}")
            if item is not None:
                setattr(config, key, item)
        env = {"uri": "BUS", "name": "SERVICE_NAME", "heartbeat_interval": "HEARTBEAT_INTERVAL"}
        for key in fields:
            variable = "CHAINS_" + env.get(key, key.upper())
            value = os.getenv(variable)
            if value is not None:
                convert = {"heartbeat_interval": int, "poll_interval": float}.get(key, str)
                try:
                    setattr(config, key, convert(value))
                except ValueError:
                    raise ValueError(f"Invalid value for {variable}") from None
        return config

    def validate(self):
        for key in ("authority", "namespace", "service_id"):
            value = getattr(self, key)
            if not isinstance(value, str) or not IDENTIFIER.fullmatch(value):
                raise ValueError(f"Invalid service {key}")
        if self.service_id == self.authority:
            raise ValueError("Service ID must differ from the authority ID")
        if not isinstance(self.name,str):
            raise ValueError("Service name must be a string")
        if self.encoding not in {"json", "cbor"}:
            raise ValueError("Service encoding must be json or cbor")
        if type(self.heartbeat_interval) is not int or not 1 <= self.heartbeat_interval <= 3600:
            raise ValueError("Heartbeat interval must be an integer from 1 to 3600 seconds")
        interval = self.poll_interval
        if type(interval) not in {int, float} or not 0 <= interval <= TIMEOUT_MAX:
            raise ValueError("Poll interval must be zero or a finite positive number of seconds within the platform wait limit")


def stable_identity(service_type, instance):
    root = xdg("XDG_STATE_HOME", ".local/state") / "chains" / "services"
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    path = root / f"{service_type}.{instance}.id"
    try:
        return path.read_text(encoding="ascii").strip()
    except FileNotFoundError:
        pass
    with NamedTemporaryFile(dir=root, delete=False, mode="w", encoding="ascii") as tmp:
        temp = Path(tmp.name)
        tmp.write(uuid4().hex + "\n")
    try:
        try:
            os.link(temp, path)
        except FileExistsError:
            pass
        return path.read_text(encoding="ascii").strip()
    finally:
        temp.unlink()
