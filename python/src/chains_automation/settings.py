"""Typed settings shared by the launcher and the service configuration loader."""

from dataclasses import MISSING, dataclass, fields, is_dataclass
import os
import re
import tomllib
from types import UnionType
from typing import Union, get_args, get_origin, get_type_hints


_NAME = re.compile(r"[a-z][a-z0-9]*(?:_[a-z0-9]+)*")


def _optional(annotation):
    args = get_args(annotation)
    if get_origin(annotation) in (Union, UnionType) and len(args) == 2 and type(None) in args:
        return next(arg for arg in args if arg is not type(None)), True
    return annotation, False


def _supported(annotation):
    annotation, _ = _optional(annotation)
    origin, args = get_origin(annotation), get_args(annotation)
    if annotation in (str, bool, int, float):
        return True
    if origin is list:
        return len(args) == 1 and _supported(args[0])
    if origin is dict:
        return len(args) == 2 and args[0] is str and _supported(args[1])
    return False


def _convert(value, annotation, label):
    annotation, nullable = _optional(annotation)
    if value is None and nullable:
        return None
    origin, args = get_origin(annotation), get_args(annotation)
    if annotation is float and type(value) in (int, float):
        try:
            return float(value)
        except OverflowError:
            raise ValueError(f"Invalid value for {label}; number out of range") from None
    if annotation in (str, bool, int) and type(value) is annotation:
        return value
    if origin is list and isinstance(value, list):
        return [_convert(item, args[0], label) for item in value]
    if origin is dict and isinstance(value, dict) and all(isinstance(key, str) for key in value):
        return {key: _convert(item, args[1], label) for key, item in value.items()}
    raise ValueError(f"Invalid value for {label}; expected {annotation}")


@dataclass(frozen=True)
class Setting:
    path: tuple[str, ...]
    annotation: type
    help: str

    @property
    def label(self):
        return ".".join(("settings", *self.path))

    @property
    def flag(self):
        return "--" + "-".join(self.path).replace("_", "-")

    @property
    def env(self):
        return "CHAINS_SETTINGS__" + "__".join(self.path).upper()

    def parse(self, raw):
        annotation, _ = _optional(self.annotation)
        if annotation is str:
            return raw
        try:
            document = tomllib.loads("value = " + raw)
        except tomllib.TOMLDecodeError:
            raise ValueError(f"Invalid TOML value for {self.label}") from None
        if document.keys() != {"value"}:
            raise ValueError(f"Expected a single TOML value for {self.label}")
        return document["value"]

    def convert(self, value):
        return _convert(value, self.annotation, self.label)


@dataclass(frozen=True)
class Section:
    schema: type | None
    children: dict

    @classmethod
    def discover(cls, schema, path=(), ancestors=()):
        if schema is None and not path:
            return cls(None, {})
        if not isinstance(schema, type) or not is_dataclass(schema) or schema in ancestors:
            raise ValueError("settings_type and nested sections must be nonrecursive dataclasses")
        hints = get_type_hints(schema)
        children = {}
        for field in fields(schema):
            if not field.init:
                continue
            name = field.name
            if not _NAME.fullmatch(name):
                raise ValueError(f"Invalid settings field name {name!r}")
            annotation = hints[name]
            if is_dataclass(annotation):
                child = cls.discover(annotation, (*path, name), (*ancestors, schema))
            elif _supported(annotation):
                child = Setting((*path, name), annotation, field.metadata.get("help", ""))
            else:
                raise ValueError(f"Unsupported type for settings.{'.'.join((*path, name))}")
            children[name] = (field, child)
        section = cls(schema, children)
        flags, variables = set(), set()
        for setting in section.leaves():
            if setting.flag in flags or setting.env in variables:
                raise ValueError(f"Conflicting name for {setting.label}")
            flags.add(setting.flag)
            variables.add(setting.env)
        return section

    def leaves(self):
        for _, child in self.children.values():
            if isinstance(child, Section):
                yield from child.leaves()
            else:
                yield child

    def merge(self, target, values, path="settings"):
        if not isinstance(values, dict):
            raise ValueError(f"Expected a table for {path}")
        for name, value in values.items():
            if name not in self.children:
                raise ValueError(f"Unknown setting {path}.{name}")
            _, child = self.children[name]
            if isinstance(child, Section):
                child.merge(target.setdefault(name, {}), value, f"{path}.{name}")
            else:
                target[name] = value

    def build(self, values, default=MISSING):
        if self.schema is None:
            return None
        arguments = {}
        for name, (field, child) in self.children.items():
            fallback = field.default
            if default is not MISSING:
                fallback = getattr(default, name)
            elif field.default_factory is not MISSING and (name not in values or isinstance(child, Section)):
                fallback = field.default_factory()
            if isinstance(child, Section):
                arguments[name] = child.build(values.get(name, {}), fallback)
                continue
            effective = values.get(name, fallback)
            if effective is MISSING:
                raise ValueError(f"Missing required setting {child.label}")
            arguments[name] = child.convert(effective)
        return self.schema(**arguments)

    def apply_environment(self, values):
        names = {leaf.env: leaf for leaf in self.leaves()}
        for variable, raw in os.environ.items():
            if not variable.startswith("CHAINS_SETTINGS__"):
                continue
            if variable not in names:
                raise ValueError(f"Unknown service settings variable {variable}")
            leaf = names[variable]
            put_setting(values, leaf.path, leaf.parse(raw))


def put_setting(values, path, value):
    for part in path[:-1]:
        values = values.setdefault(part, {})
    values[path[-1]] = value
