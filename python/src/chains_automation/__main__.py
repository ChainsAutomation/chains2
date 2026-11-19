import argparse
from importlib import import_module
import os
import sys

from .service import Service
from .services import BUNDLED_SERVICES
from .settings import Section, put_setting


def main(argv=None):
    args = sys.argv[1:] if argv is None else argv
    parser = argparse.ArgumentParser(description="Run a Chains Python service", allow_abbrev=False)
    parser.add_argument("service", metavar="NAME|MODULE:CLASS", nargs="?",
                        help="Bundled short name or custom Service subclass")
    parser.add_argument("--list", action="store_true", help="List bundled services")
    parser.add_argument("-f", "--config", dest="config_path", help="Path to a service TOML file")
    parser.add_argument("--bus", dest="uri", help="MQTT or Redis broker URI")
    parser.add_argument("--namespace")
    parser.add_argument("--encoding", choices=("json", "cbor"))
    parser.add_argument("--authority")
    parser.add_argument("--heartbeat-interval", type=int, metavar="SECONDS")
    parser.add_argument("--poll-interval", type=float, metavar="SECONDS", help="Zero disables polling")
    parser.add_argument("--service-id")
    parser.add_argument("--service-name", dest="name")
    parser.add_argument("--instance")
    parser.add_argument("--emit", metavar="EVENT", help="Invoke one event method that takes no required arguments")
    if args == ["--list"]:
        for name in sorted(BUNDLED_SERVICES):
            print(name)
        return
    if args in (["-h"], ["--help"]):
        parser.print_help()
        return
    if not args:
        parser.error("specify a service or use --list")
    target = args[0]
    if target.startswith("-"):
        parser.error("put the service name before its options, or use --list")
    spec = BUNDLED_SERVICES.get(target, target)
    module, separator, name = spec.partition(":")
    if not separator:
        parser.error(f"unknown bundled service {target!r}; use --list or specify MODULE:CLASS")
    if not name.isidentifier() or not all(part.isidentifier() for part in module.split(".")):
        parser.error("expected a service in MODULE:CLASS form")
    try:
        service = getattr(import_module(module), name)
    except (ImportError, AttributeError) as exc:
        parser.error(str(exc))
    if not isinstance(service, type) or not issubclass(service, Service) or service is Service:
        parser.error("MODULE:CLASS must identify a Service subclass")
    try:
        schema = Section.discover(service.settings_type)
        for i, setting in enumerate(schema.leaves()):
            try:
                parser.add_argument(setting.flag, dest=f"_setting_{i}", metavar="VALUE",
                                    help=setting.help or setting.label)
            except argparse.ArgumentError:
                raise ValueError(f"Setting {setting.label} conflicts with {setting.flag}") from None
        options = vars(parser.parse_args(args))
        if options.pop("list"):
            parser.error("use --list on its own")
        options.pop("service")
        emit = options.pop("emit")
        settings = {}
        for i, setting in enumerate(schema.leaves()):
            raw = options.pop(f"_setting_{i}")
            if raw is not None and setting.env not in os.environ:
                put_setting(settings, setting.path, setting.parse(raw))
        kwargs = {key: value for key, value in options.items() if value is not None}
        if settings:
            kwargs["settings"] = settings
        service(**kwargs).run(emit=emit)
    except (ValueError, OSError) as exc:
        parser.exit(1, f"{parser.prog}: {exc}\n")


def console_main():
    if not sys.flags.safe_path:
        sys.path.insert(0, os.getcwd())
    main()


if __name__ == "__main__":
    main()
