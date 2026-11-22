# Household dashboards

Open `/ui/` for the shared home dashboard. The top bar also links to **All Devices** and any dashboards pinned by an operator. Cards show reported device values, including power and brightness for lights with those capabilities. A pending control stays separate from the reported value until the device sends an update.

## Create and share a dashboard

1. Sign in as an operator and open **Dashboard builder**.
2. Create a dashboard and add individual devices, live groups, or separators. Use the move buttons to set the order. The grid fits the screen while preserving that order.
3. Select **Use as home** to make it the front page or **Pin in navigation** to add a link to the top bar. Select **Save dashboards** to publish.

Live groups can combine a location pattern, device class, and required tags. Enter one location segment per line. For example, `House`, `Bedroom`, and `**` include the bedroom and its descendants. All tags must match. Fixed cards take priority over group matches. The first matching group gets a device when several groups overlap. Groups update as devices register and their placement changes. [Device matching](devices.md)

To give household users access, set `CHAINS_HOUSEHOLD_TOKEN` (or the `household_token` setting or `-household-token` flag) on the web-enabled authority. Use a token distinct from your operator token. Open **All Devices**, select **Edit placement** on each device you want to share, and enable **Share with household**. Sharing gives household users access to that device’s reported attributes and typed controls. Use **Preview as household** in the builder to see which cards they can access.

Household users sign in with their token. They can open saved dashboards and browse or control shared devices. The operator token is required for the builder, placement, and operator tools. Use the same HTTPS and browser-session setup as the operator console. [Web console setup](operator-web.md)

The authority stores layouts, navigation pins, and the home selection in its SQLite database. Back up that database to preserve dashboards and sharing settings. A memory-only database loses these edits on restart.
