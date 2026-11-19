from __future__ import annotations

from queue import Empty, Full, Queue
from threading import Event
from urllib.parse import unquote, urlsplit
from uuid import uuid4

from .client import Delivery, IDENTIFIER, MAX_PAYLOAD


def check_routes(namespace: str, types: tuple[str, ...]) -> None:
    if not IDENTIFIER.fullmatch(namespace) or any(not IDENTIFIER.fullmatch(kind) for kind in types):
        raise ValueError("invalid namespace or message type")


class MQTT:
    def __init__(self, address: str, namespace: str = "chains", types: tuple[str, ...] = ()):
        from paho.mqtt import client as mqtt

        check_routes(namespace, types)
        url = urlsplit(address)
        unix = url.scheme == "mqtt+unix"
        if unix:
            if url.netloc or not url.path.startswith("/"):
                raise ValueError("mqtt+unix requires an absolute socket path")
        elif url.scheme not in {"mqtt", "mqtts"} or not url.hostname:
            raise ValueError("expected mqtt:// or mqtts:// broker URL")

        self._prefix = namespace + "/"
        self._queue: Queue[Delivery] = Queue(maxsize=1024)
        self._overflow = Event()
        self._ready = Event()
        self._connected = False
        self._error: str | None = None
        self._client = mqtt.Client(
            mqtt.CallbackAPIVersion.VERSION2,
            client_id="chains-" + uuid4().hex,
            protocol=mqtt.MQTTv5,
            transport="unix" if unix else "tcp",
        )
        filters = [(self._prefix + "msg/" + kind + "/#", 1) for kind in dict.fromkeys(types)]
        if not filters:
            filters = [(self._prefix + "msg/#", 1)]

        def on_connect(client, userdata, flags, reason, properties):
            self._ready.clear()
            if reason.is_failure:
                self._error = str(reason)
                self._ready.set()
                return
            code, _ = client.subscribe(filters)
            if code != mqtt.MQTT_ERR_SUCCESS:
                self._error = str(code)
                self._ready.set()

        def on_subscribe(client, userdata, mid, reasons, properties):
            if any(reason.is_failure for reason in reasons):
                self._error = "MQTT subscription refused"
            else:
                self._error = None
                if self._connected:
                    self._offer(Delivery(reset=True))
                self._connected = True
            self._ready.set()

        def on_message(client, userdata, packet):
            if len(packet.payload) <= MAX_PAYLOAD and packet.topic.startswith(self._prefix):
                self._offer(Delivery(packet.topic[len(self._prefix):], packet.payload, retained=packet.retain))

        self._client.on_connect = on_connect
        self._client.on_subscribe = on_subscribe
        self._client.on_message = on_message
        if url.username is not None:
            self._client.username_pw_set(unquote(url.username), unquote(url.password or ""))
        if url.scheme == "mqtts":
            self._client.tls_set()
        host = unquote(url.path) if unix else url.hostname
        port = url.port or (8883 if url.scheme == "mqtts" else 1883)
        self._client.connect(host, port, keepalive=30, clean_start=True)
        self._client.loop_start()
        if not self._ready.wait(8) or self._error:
            self.close()
            raise ConnectionError(self._error or "MQTT subscription timed out")

    def _offer(self, delivery: Delivery) -> None:
        try:
            self._queue.put_nowait(delivery)
        except Full:
            self._overflow.set()

    def publish(self, topic: str, data: bytes) -> None:
        if len(data) > MAX_PAYLOAD:
            raise ValueError("message exceeds maximum payload")
        publication = self._client.publish(self._prefix + topic, data, qos=1, retain=False)
        publication.wait_for_publish(timeout=4)
        if not publication.is_published():
            raise TimeoutError("MQTT publication timed out")

    def receive(self, timeout: float) -> Delivery | None:
        if self._overflow.is_set():
            self._overflow.clear()
            while True:
                try:
                    self._queue.get_nowait()
                except Empty:
                    break
            return Delivery(reset=True)
        try:
            return self._queue.get(timeout=timeout)
        except Empty:
            return None

    def close(self) -> None:
        self._client.disconnect()
        self._client.loop_stop()


APPEND_STREAMS = """
local id = redis.call("XADD", KEYS[1], "MAXLEN", "~", "10000", "*", "topic", ARGV[1], "data", ARGV[2])
return redis.call("XADD", KEYS[2], "MAXLEN", "~", "10000", id, "topic", ARGV[1], "data", ARGV[2])
"""


class RedisStreams:
    def __init__(self, address: str, namespace: str = "chains", types: tuple[str, ...] = ()):
        from redis import Redis
        from redis.exceptions import ConnectionError as RedisConnectionError, TimeoutError as RedisTimeout

        check_routes(namespace, types)
        self._connection_errors = (RedisConnectionError, RedisTimeout)
        self._client = Redis.from_url(address, socket_timeout=4, socket_connect_timeout=3)
        self._prefix = namespace + ":"
        self._keys = [self._prefix + "type:" + kind for kind in dict.fromkeys(types)]
        if not self._keys:
            self._keys = [self._prefix + "all"]
        self._cursor = self._heads()
        self._disconnected = False

    def _heads(self) -> dict[str, bytes]:
        rows = self._client.xrevrange(self._prefix + "all", count=1)
        head = rows[0][0] if rows else b"0-0"
        return dict.fromkeys(self._keys, head)

    def publish(self, topic: str, data: bytes) -> None:
        if len(data) > MAX_PAYLOAD:
            raise ValueError("message exceeds maximum payload")
        kind = topic.split("/")[1]
        self._client.eval(
            APPEND_STREAMS, 2, self._prefix + "all", self._prefix + "type:" + kind, topic, data,
        )

    def receive(self, timeout: float) -> Delivery | None:
        try:
            if self._disconnected:
                self._cursor = self._heads()
                self._disconnected = False
                return Delivery(reset=True)
            streams = self._client.xread(self._cursor, count=1, block=max(1, int(timeout * 1000)))
        except self._connection_errors:
            self._disconnected = True
            return None
        if not streams:
            return None
        key, (ident, fields) = min(
            ((key, row) for key, rows in streams for row in rows),
            key=lambda item: tuple(map(int, item[1][0].split(b"-"))),
        )
        self._cursor[key.decode()] = ident
        return Delivery(fields[b"topic"].decode(), fields[b"data"])

    def close(self) -> None:
        self._client.close()
