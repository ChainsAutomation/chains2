"""Bundled service names mapped to lazily imported MODULE:CLASS targets."""

BUNDLED_SERVICES: dict[str, str] = {
    "frigate": "chains_automation.services.frigate:Frigate",
    "radicale": "chains_automation.services.radicale:Radicale",
    "rflink": "chains_automation.services.rflink:RFLink",
    "tibber-pulse": "chains_automation.services.tibber_pulse:TibberPulse",
}
