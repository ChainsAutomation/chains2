from dataclasses import dataclass
import tomllib
import os
from pathlib import Path
from tempfile import NamedTemporaryFile
from threading import TIMEOUT_MAX
from uuid import uuid4

from .client import IDENTIFIER
from .settings import Section


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
    instance: str = ""

    @classmethod
    def load(cls, service_type, path=None, *, settings_type=None, settings=None, **overrides):
        explicit = os.environ.get("CHAINS_SERVICE_CONFIG") if "CHAINS_SERVICE_CONFIG" in os.environ else path
        explicit = Path(explicit) if explicit else None
        instance_override = overrides.pop("instance", None)
        fallback = explicit.stem if explicit is not None else service_type
        instance = os.environ.get("CHAINS_INSTANCE", instance_override if instance_override is not None else fallback)
        if not isinstance(instance, str) or not IDENTIFIER.fullmatch(instance) or instance in {".", ".."}:
            raise ValueError("Invalid Chains service instance")
        config = cls(instance=instance, name=instance)
        if explicit is not None:
            paths = [explicit]
        else:
            system = os.environ.get("XDG_CONFIG_DIRS", "/etc/xdg").split(":")
            roots = [Path(root) for root in reversed(system) if Path(root).is_absolute()]
            roots.append(xdg("XDG_CONFIG_HOME", ".config"))
            paths = [root / "chains" / "services" / f"{instance}.toml" for root in roots]
        fields = set(cls.__dataclass_fields__) - {"instance"}
        schema = Section.discover(settings_type)
        values = {}
        for file in paths:
            try:
                with file.open("rb") as stream:
                    document = tomllib.load(stream)
            except FileNotFoundError:
                if explicit is not None:
                    raise
                continue
            except (tomllib.TOMLDecodeError, UnicodeDecodeError):
                raise ValueError(f"Invalid TOML in {file}") from None
            unknown = document.keys() - {"service", "settings"}
            if unknown:
                raise ValueError(f"Unknown config section {min(unknown)!r} in {file}")
            common = document.get("service", {})
            if not isinstance(common, dict):
                raise ValueError(f"Expected a [service] table in {file}")
            unknown = common.keys() - fields
            if unknown:
                raise ValueError(f"Unknown common setting service.{min(unknown)} in {file}")
            for key, item in common.items():
                setattr(config, key, item)
            schema.merge(values, document.get("settings", {}))
        for key, item in overrides.items():
            if key not in fields:
                raise TypeError(f"Unknown service option {key}")
            if item is not None:
                setattr(config, key, item)
        env = {"uri": "BUS", "name": "SERVICE_NAME"}
        for key in fields:
            variable = "CHAINS_" + env.get(key, key.upper())
            value = os.getenv(variable)
            if value is not None:
                convert = {"heartbeat_interval": int, "poll_interval": float}.get(key, str)
                try:
                    setattr(config, key, convert(value))
                except ValueError:
                    raise ValueError(f"Invalid value for {variable}") from None
        schema.merge(values, {} if settings is None else settings)
        schema.apply_environment(values)
        return config, schema.build(values)

    def validate(self):
        if not isinstance(self.uri, str) or not self.uri:
            raise ValueError("Service URI must be a nonempty string")
        for key in ("authority", "namespace", "service_id"):
            value = getattr(self, key)
            if not isinstance(value, str) or not IDENTIFIER.fullmatch(value):
                raise ValueError(f"Invalid service {key}")
        if self.service_id == self.authority:
            raise ValueError("Service ID must differ from the authority ID")
        if not isinstance(self.name, str):
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
