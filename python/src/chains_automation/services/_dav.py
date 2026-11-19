"""WebDAV transport shared by calendar and address book synchronization."""
from base64 import b64encode
from dataclasses import dataclass
from urllib.error import HTTPError
from urllib.parse import unquote, urljoin, urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener
from xml.etree.ElementTree import Element, SubElement, tostring

from defusedxml.ElementTree import fromstring


DAV = "{DAV:}"
CAL = "{urn:ietf:params:xml:ns:caldav}"
CARD = "{urn:ietf:params:xml:ns:carddav}"
MAX_RESPONSE = 16 << 20


class DAVError(RuntimeError):
    def __init__(self, status, method, body=b""):
        super().__init__(f"Radicale {method} returned HTTP {status}")
        self.status = status
        self.invalid_token = False
        if status == 403:
            try:
                self.invalid_token = fromstring(body).find(DAV + "valid-sync-token") is not None
            except (SyntaxError, ValueError):
                pass


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


@dataclass
class Listing:
    token: str
    changes: dict[str, str | None]
    complete: bool


class DAVClient:
    def __init__(self, base, username, password):
        self.base = base.rstrip("/") + "/"
        url = urlsplit(self.base)
        if url.scheme not in {"http", "https"} or not url.hostname or url.username or url.query or url.fragment:
            raise ValueError("Radicale URL must be an HTTP(S) base URL without credentials, query or fragment")
        self.origin = (url.scheme, url.netloc)
        self.auth = "Basic " + b64encode(f"{username}:{password}".encode()).decode() if username else ""
        self.opener = build_opener(NoRedirect)

    def url(self, parent, path):
        result = urljoin(parent, path)
        parsed = urlsplit(result)
        decoded = unquote(parsed.path)
        root = unquote(urlsplit(parent).path)
        if ((parsed.scheme, parsed.netloc) != self.origin or parsed.query or parsed.fragment
                or not decoded.startswith(root) or "\\" in decoded
                or any(part in {".", ".."} for part in decoded.split("/"))):
            raise ValueError("DAV resource must remain inside its configured collection")
        return result

    def request(self, method, url, data=None, **headers):
        if self.auth:
            headers["Authorization"] = self.auth
        req = Request(url, data, method=method, headers=headers)
        try:
            with self.opener.open(req, timeout=8) as response:
                raw = response.read(MAX_RESPONSE + 1)
                if len(raw) > MAX_RESPONSE:
                    raise ValueError("Radicale response exceeds 16 MiB")
                return raw, response.headers
        except HTTPError as error:
            with error:
                raise DAVError(error.code, method, error.read(65536)) from None

    def get(self, url):
        raw, headers = self.request("GET", url)
        return raw, headers.get("ETag")

    def listing(self, collection, token):
        query = Element(DAV + "sync-collection")
        SubElement(query, DAV + "sync-token").text = token
        SubElement(query, DAV + "sync-level").text = "1"
        SubElement(SubElement(query, DAV + "prop"), DAV + "getetag")
        try:
            tree = self._xml("REPORT", collection, query, "0")
        except DAVError as error:
            if not (error.invalid_token or error.status in {400, 405, 501}):
                raise
            if error.invalid_token and token:
                return self.listing(collection, "")
            return self._full_listing(collection)
        next_token = tree.findtext(DAV + "sync-token")
        if not next_token:
            raise ValueError("Radicale sync response has no continuation token")
        return Listing(next_token, self._items(tree, collection), not token)

    def resource_kind(self, collection):
        query = Element(DAV + "propfind")
        SubElement(SubElement(query, DAV + "prop"), DAV + "resourcetype")
        tree = self._xml("PROPFIND", collection, query, "0")
        for props in self._properties(tree):
            resource = props.find(DAV + "resourcetype")
            if resource is not None:
                if resource.find(CAL + "calendar") is not None:
                    return "calendar"
                if resource.find(CARD + "addressbook") is not None:
                    return "contact"
        raise ValueError("Configured DAV path is not a calendar or address book")

    def _full_listing(self, collection):
        query = Element(DAV + "propfind")
        SubElement(SubElement(query, DAV + "prop"), DAV + "getetag")
        tree = self._xml("PROPFIND", collection, query, "1")
        return Listing("", self._items(tree, collection), True)

    def _xml(self, method, url, query, depth):
        raw, _ = self.request(method, url, tostring(query, encoding="utf-8"),
                              Depth=depth, **{"Content-Type": "application/xml"})
        tree = fromstring(raw)
        if tree.tag != DAV + "multistatus":
            raise ValueError("Expected a DAV multistatus response")
        return tree

    @staticmethod
    def _status(element):
        value = element.findtext(DAV + "status", "").split()
        return int(value[1]) if len(value) > 1 and value[1].isdigit() else 0

    @classmethod
    def _properties(cls, tree):
        for status in tree.iter(DAV + "propstat"):
            if cls._status(status) == 200:
                prop = status.find(DAV + "prop")
                if prop is not None:
                    yield prop

    def _items(self, tree, collection):
        items = {}
        for response in tree.findall(DAV + "response"):
            href = response.findtext(DAV + "href")
            if not href:
                raise ValueError("DAV response is missing an href")
            url = self.url(collection, href)
            if url == collection:
                if self._status(response) not in {0, 200}:
                    raise ValueError("Incomplete DAV collection listing")
                continue
            status = self._status(response)
            if status in {404, 410}:
                items[url] = None
            elif status in {0, 200}:
                tags = [props.findtext(DAV + "getetag") for props in self._properties(response)]
                if not any(tags):
                    raise ValueError("DAV item is missing an ETag")
                items[url] = next(tag for tag in tags if tag)
            else:
                raise ValueError("Incomplete DAV item listing")
        return items
