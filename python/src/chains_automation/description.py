from __future__ import annotations

from dataclasses import dataclass
from inspect import Parameter, Signature, signature
from types import UnionType
from typing import Any, Literal, Union, get_args, get_origin, get_type_hints

from jsonschema import Draft202012Validator

from .client import IDENTIFIER


def capability(*, name=None, version="1", schema=None, device=None, idempotent=False):
    """Override metadata on an event_ or action_ method."""
    def decorate(method):
        method.__chains_capability__ = {
            "name": name, "version": version, "schema": schema,
            "device": device, "idempotent": idempotent,
        }
        return method
    return decorate


def annotation_schema(annotation):
    if annotation in (Any, Parameter.empty):
        return {}
    scalars = {str: "string", int: "integer", float: "number", bool: "boolean", type(None): "null"}
    if annotation in scalars:
        return {"type": scalars[annotation]}
    origin, args = get_origin(annotation), get_args(annotation)
    if origin in (Union, UnionType):
        return {"anyOf": [annotation_schema(arg) for arg in args]}
    if origin is Literal:
        return {"enum": list(args)}
    if annotation is list or origin is list:
        return {"type": "array", "items": annotation_schema(args[0]) if args else {}}
    if annotation is dict or origin is dict and args[0] is str:
        return {"type": "object", "additionalProperties": annotation_schema(args[1]) if args else True}
    raise TypeError(f"Unsupported annotation {annotation!r}; provide an explicit schema")


@dataclass(frozen=True)
class Capability:
    name: str
    version: str
    device: str | None
    idempotent: bool
    signature: Signature
    validator: Draft202012Validator

    @classmethod
    def discover(cls, suffix, method):
        options = getattr(method, "__chains_capability__", {})
        name, version = options.get("name") or suffix, options.get("version", "1")
        if not IDENTIFIER.fullmatch(name) or not IDENTIFIER.fullmatch(version):
            raise ValueError(f"Invalid capability name or version for {method.__name__}")
        sig = signature(method)
        for param in sig.parameters.values():
            if param.kind not in {Parameter.POSITIONAL_OR_KEYWORD, Parameter.KEYWORD_ONLY}:
                raise TypeError(f"{method.__name__} requires named, fixed parameters")
        schema = options.get("schema")
        if schema is None:
            hints = get_type_hints(method)
            properties = {p: annotation_schema(hints.get(p, Parameter.empty)) for p in sig.parameters}
            required = [p.name for p in sig.parameters.values() if p.default is Parameter.empty]
            schema = {"type": "object", "properties": properties, "required": required,
                      "additionalProperties": False}
        Draft202012Validator.check_schema(schema)
        return cls(name, version, options.get("device"), options.get("idempotent", False),
                   sig, Draft202012Validator(schema))
