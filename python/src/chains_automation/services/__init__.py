"""Bundled service names mapped to lazily imported MODULE:CLASS targets."""

BUNDLED_SERVICES: dict[str, str] = {
    "frigate": "chains_automation.services.frigate:Frigate",
    "rflink": "chains_automation.services.rflink:RFLink",
}
