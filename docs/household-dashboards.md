# Household dashboards

Open `/` for the shared home dashboard. The top bar also links to **All Devices** and any dashboards pinned by an operator. Cards show reported device values, including power and brightness for lights with those capabilities. A pending control stays separate from the reported value until the device sends an update.

## Create and share a dashboard

1. Sign in to the household app with the household or operator token. Select **Create dashboard** on Home or **New dashboard** under **Dashboards**.
2. Name the dashboard and add devices, live groups, or separators. Use the move buttons to set the order and check the layout in the live preview.
3. Select **Use as home** to make it the front page or **Pin in navigation** to add a top-bar link. **Use as home** starts selected if no home dashboard exists. Select **Save dashboard** to open the saved page. Find every saved dashboard under **Dashboards**, including unpinned ones.

Live groups can combine a location pattern, device class, and required tags. Choose a location from the catalog or enter one segment per line under **Advanced location pattern**. For example, `House`, `Bedroom`, and `**` include the bedroom and its descendants. All tags must match. Fixed cards take priority over group matches. The first matching group gets a device when several groups overlap. Groups update as devices register and their placement changes. [Device matching](devices.md)

To give household users access, set `CHAINS_HOUSEHOLD_TOKEN` (or the `household_token` setting or `-household-token` flag) on the authority. Enable the app with `-household-web`, `household_web`, or `CHAINS_HOUSEHOLD_WEB=true`. Use a token distinct from your operator token.

To load the household token from a helper, set `household_token_command` in the engine JSON config to an array of executable and arguments. The engine resolves it once at startup when the household app is enabled. `CHAINS_HOUSEHOLD_TOKEN` and `-household-token` override the configured source. See [token command syntax](operator-cli.md#configure-the-authority).

Household users sign in with their token. They can open saved dashboards, see every registered device in **All Devices**, and use its declared controls. This includes reported data such as calendar and camera attributes. Both tokens can manage the same shared dashboard catalog. The operator token is required for rules, traces, and placement editing. The operator console at `/ui/` has its own sign-in. Both apps use the same HTTPS requirements for browser sessions. [Web console setup](operator-web.md)

The authority stores layouts, navigation pins, and the home selection in its SQLite database. Back up that database to preserve dashboards. A memory-only database loses these edits on restart.
