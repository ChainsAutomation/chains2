from contextlib import redirect_stderr, redirect_stdout
from dataclasses import dataclass, field
from io import StringIO
import os
from pathlib import Path
from tempfile import TemporaryDirectory
from types import ModuleType
from unittest import TestCase
from unittest.mock import patch

from chains_automation import Service
from chains_automation.__main__ import main
from chains_automation.services import BUNDLED_SERVICES


@dataclass
class Metrics:
    url: str = field(metadata={"help": "Metrics server address"})
    interval: float = 30
    enabled: bool = True
    hosts: list[str] = field(default_factory=list)
    quotas: dict[str, int] = field(default_factory=dict)
    token: str | None = None


@dataclass
class Settings:
    metrics: Metrics


class Listener(Service):
    settings_type = Settings


class ConfigTest(TestCase):
    def setUp(self):
        self.directory = TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.home = self.root / "home"
        self.system = self.root / "system"
        environment = patch.dict(os.environ, {
            "XDG_CONFIG_HOME": str(self.home),
            "XDG_CONFIG_DIRS": str(self.system),
            "XDG_STATE_HOME": str(self.root / "state"),
        }, clear=True)
        environment.start()
        self.addCleanup(environment.stop)

    def config(self, name, content, *, root=None):
        path = (root or self.home) / "chains" / "services" / f"{name}.toml"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")
        return path

    def test_sections_and_overrides(self):
        self.config("listener", '[settings.metrics]\nurl = "system"\ninterval = 12\n'
                    'quotas = {west = 1, east = 2}\n', root=self.system)
        self.config("listener", '[service]\nnamespace = "mine"\n[settings.metrics]\ninterval = 8\n'
                    'hosts = ["local"]\n')
        with patch.dict(os.environ, {"CHAINS_SETTINGS__METRICS__INTERVAL": "3.5",
                                     "CHAINS_SETTINGS__METRICS__HOSTS": '["env"]',
                                     "CHAINS_SETTINGS__METRICS__ENABLED": "false"}):
            service = Listener(settings={"metrics": {"interval": 4, "quotas": {"north": 3}}})
        self.assertEqual(service.settings.metrics, Metrics("system", 3.5, False, ["env"], {"north": 3}))
        self.assertEqual(service.config.namespace, "mine")

    def test_instances_have_distinct_identity_and_stable_names(self):
        west = self.config("west", '[settings.metrics]\nurl = "west"\n')
        east = self.config("east", '[settings.metrics]\nurl = "east"\n')
        first = Listener(config_path=west).describe()
        self.assertEqual(first, Listener(instance="west").describe())
        second = Listener(config_path=east).describe()
        self.assertNotEqual(first["uuid"], second["uuid"])
        self.assertEqual((first["name"], second["name"]), ("west", "east"))
        renamed = Listener(config_path=west, name="West Metrics").describe()
        self.assertEqual(renamed["uuid"], first["uuid"])
        self.assertEqual(renamed["name"], "West Metrics")
        with patch.dict(os.environ, {"CHAINS_INSTANCE": "manual", "CHAINS_SERVICE_CONFIG": str(east)}):
            service = Listener(config_path=west, instance="other")
            self.assertEqual((service.config.instance, service.settings.metrics.url), ("manual", "east"))

    def test_config_diagnostics_do_not_expose_values(self):
        with self.assertRaisesRegex(ValueError, "Missing required setting settings.metrics.url"):
            Listener()
        self.config("listener", '[settings.metrics]\nurl = "configured"\ninterval = "secret"\n')
        with self.assertRaises(ValueError) as failure:
            Listener()
        self.assertIn("settings.metrics.interval", str(failure.exception))
        self.assertNotIn("secret", str(failure.exception))
        self.config("listener", '[settings.metrics]\nurl = "configured"\nmisspelled = 1\n')
        with self.assertRaisesRegex(ValueError, "Unknown setting settings.metrics.misspelled"):
            Listener()
        self.config("listener", '[service]\nencoding = "json"\n[settings.metrics]\nurl = "configured"\n')
        with patch.dict(os.environ, {"CHAINS_SETTINGS__METRICS__UNKNOWN": "secret"}):
            with self.assertRaisesRegex(ValueError, "Unknown service settings variable"):
                Listener()

    def test_bundle_listing_is_lazy_and_launch_generates_flags(self):
        launched = []

        class Fixture(Listener):
            def run(self, *, emit=None):
                launched.append((self.config, self.settings, emit))

        module = ModuleType("test_bundle")
        module.Fixture = Fixture
        output = StringIO()
        with patch.dict(BUNDLED_SERVICES, {"fixture": "test_bundle:Fixture", "other": "unimportable:Service"}, clear=True):
            with redirect_stdout(output):
                main(["--list"])
            self.assertEqual(output.getvalue().splitlines(), ["fixture", "other"])
            with patch.dict("sys.modules", {"test_bundle": module}):
                output = StringIO()
                with redirect_stdout(output), self.assertRaises(SystemExit) as result:
                    main(["fixture", "--help"])
                self.assertEqual(result.exception.code, 0)
                self.assertIn("Metrics server address", output.getvalue())
                file = self.config("fixture-west", '[settings.metrics]\nurl = "from-file"\n')
                main(["fixture", "-f", str(file), "--metrics-url", "from-cli",
                      "--metrics-hosts", '["west", "east"]', "--emit", "ping"])
                config, settings, emit = launched.pop()
                self.assertEqual((config.instance, emit), ("fixture-west", "ping"))
                self.assertEqual(settings.metrics, Metrics("from-cli", hosts=["west", "east"]))
                with patch.dict(os.environ, {"CHAINS_SETTINGS__METRICS__INTERVAL": "8"}):
                    main(["test_bundle:Fixture", "-f", str(file), "--metrics-interval", "overridden"])
                self.assertEqual(launched.pop()[1].metrics.interval, 8)

    def test_flag_collision_diagnostic(self):
        @dataclass
        class Conflict:
            poll_interval: float = 1

        class Fixture(Service):
            settings_type = Conflict

        module = ModuleType("test_conflict")
        module.Fixture = Fixture
        output = StringIO()
        with patch.dict("sys.modules", {"test_conflict": module}), redirect_stderr(output):
            with self.assertRaises(SystemExit):
                main(["test_conflict:Fixture", "--help"])
        self.assertIn("conflicts with --poll-interval", output.getvalue())
