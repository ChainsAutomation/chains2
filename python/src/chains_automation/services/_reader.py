"""Lifecycle for bundled services with an independent blocking input stream."""

import logging
from threading import Event, Thread, TIMEOUT_MAX

from ..service import Service


log = logging.getLogger(__name__)


class ReaderService(Service):
    def onStart(self):
        self._source_stop = Event()
        self._source_thread = Thread(target=self._read_reconnecting, name=f"chains-{self.service_type}-input")
        self._source_thread.start()

    def onShutdown(self):
        if hasattr(self, "_source_thread"):
            self._source_stop.set()
            self._source_thread.join()

    def validate_reconnect_interval(self, interval):
        if not 0 < interval <= TIMEOUT_MAX:
            raise ValueError(f"{self.service_type} reconnect_interval must be a positive, finite duration")

    def _read_reconnecting(self):
        while not self.stopping:
            try:
                self.read_source()
            except (OSError, ValueError, RuntimeError) as exc:
                if not self.stopping:
                    log.warning("%s input disconnected (%s); retrying",
                                self.service_type, type(exc).__name__)
            if self._source_stop.wait(self.settings.reconnect_interval):
                return

    def publish_input(self, handler, payload):
        try:
            handler(**payload)
        except (OSError, ValueError, RuntimeError) as exc:
            if not self.stopping:
                log.warning("Dropping %s event after publication failure (%s)",
                            self.service_type, type(exc).__name__)

    def read_source(self):
        raise NotImplementedError
