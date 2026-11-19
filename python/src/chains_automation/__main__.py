import argparse
from importlib import import_module
import os
import sys

from .service import Service


def main(argv=None):
    parser = argparse.ArgumentParser(description="Run a Chains Python service")
    parser.add_argument("service", metavar="MODULE:CLASS", help="Service subclass to run")
    parser.add_argument("--config", dest="config_path", help="Path to a service config file")
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
    options = vars(parser.parse_args(argv))
    module, separator, name = options.pop("service").partition(":")
    emit = options.pop("emit")
    if not separator or not name.isidentifier() or not all(part.isidentifier() for part in module.split(".")):
        parser.error("expected a service in MODULE:CLASS form")
    try:
        service = getattr(import_module(module), name)
    except (ImportError, AttributeError) as exc:
        parser.error(str(exc))
    if not isinstance(service, type) or not issubclass(service, Service) or service is Service:
        parser.error("MODULE:CLASS must identify a Service subclass")
    try:
        service(**{key: value for key, value in options.items() if value is not None}).run(emit=emit)
    except (ValueError, OSError) as exc:
        parser.exit(1, f"{parser.prog}: {exc}\n")


def console_main():
    if not sys.flags.safe_path:
        sys.path.insert(0, os.getcwd())
    main()


if __name__ == "__main__":
    main()
