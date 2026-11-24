# SD2NAS

SD2NAS imports SD cards onto an Unraid NAS as soon as they are inserted. It copies new photos and videos into a staging folder, skips anything already imported or archived, and tells you when the card is safe to format. A phone-friendly web UI then lets you merge, rename, cull and promote staging folders into the archive. The design is in [docs/PLAN.md](docs/PLAN.md).

## Install on Unraid

Docker → Template repositories: add `https://github.com/aspiziri/SD2NAS`, then Add Container → SD2NAS.

Or download the template directly, then Add Container → Template → `SD2NAS`:

```
curl -fsSL https://raw.githubusercontent.com/aspiziri/SD2NAS/main/deploy/unraid/sd2nas.xml -o /boot/config/plugins/dockerMan/templates-user/my-SD2NAS.xml
```

Paths:

- `/cards` = `/mnt/disks` (RW/Slave), where Unassigned Devices mounts cards.
- `/photos` = your photos share. It holds `Archive/`; SD2NAS creates `Staging/` and `Management/SD2NAS/` next to it.
- `/config` = appdata on an exclusive share, or a pool path such as `/mnt/<pool>/appdata/sd2nas`.

`TZ` comes from Unraid. After starting, open `http://<unraid-ip>:<port>/healthz` to check the time zone, user and mounts.

## Card script

The script runs when Unassigned Devices mounts a card. It imports the card, unmounts it and sends an Unraid notification.

1. Install **Unassigned Devices** and **Unassigned Devices Plus** from Community Apps.
2. Install the script:

   ```
   curl -fsSL https://raw.githubusercontent.com/aspiziri/SD2NAS/main/deploy/unassigned-devices.sh -o /boot/config/plugins/unassigned.devices/sd2nas.sh
   ```

3. Tell it where SD2NAS listens. Change `4040` to the container's host port, the left-hand number in Docker's port mapping (`4040->8080` means 4040):

   ```
   echo 'SD2NAS_URL=http://localhost:4040' > /boot/config/plugins/unassigned.devices/sd2nas.cfg
   ```

   Check it: `. /boot/config/plugins/unassigned.devices/sd2nas.cfg; curl -sS "$SD2NAS_URL/healthz"` prints SD2NAS's health text.

4. Insert a card into **each slot** of the reader once. In Main → Unassigned Devices, open that slot's settings and set:
   - **Automount** on;
   - **Read Only** on;
   - **Share** off;
   - **Enable Device Script** and **Run Device Script File in the background** on;
   - replace everything in **Device Script content** with these two lines, then Save:

     ```
     #!/bin/bash
     exec bash /boot/config/plugins/unassigned.devices/sd2nas.sh
     ```

   Unassigned Devices names each slot's script file after the device and doesn't let you change its path, so this wrapper hands the card to `sd2nas.sh`. Its settings are per slot, so repeat this for every slot. Updating `sd2nas.sh` updates every slot.
5. Insert a card. You get `<card>: N new, M already known. Card safe to format.` and the card is unmounted.

## Notifications

- Settings → Notification Settings: enable your agent (Pushover, ntfy, Telegram...) and tick **Agents** for **Notices, Warnings and Alerts**. Successful imports are Notices.
- SD2NAS's own notices (stale staging folders after 09:00 container time, a failed nightly backup, files missing from the archive) are delivered after each card and by a **User Scripts** entry. In Community Apps → User Scripts, add a new script named `SD2NAS notices` with this content, on the schedule **Hourly**:

  ```
  #!/bin/bash
  ACTION=NOTIFY bash /boot/config/plugins/unassigned.devices/sd2nas.sh
  ```

- Without User Scripts, create `/boot/config/plugins/dynamix/sd2nas.cron` containing the line below, then run `update_cron`:

  ```
  0 * * * * ACTION=NOTIFY bash /boot/config/plugins/unassigned.devices/sd2nas.sh >/dev/null 2>&1
  ```

- To see what is waiting: `curl -s http://localhost:<port>/api/notifications`.

## Without the host script (card poller)

Set the template's **Card poller** to `true` (`SD2NAS_POLL_CARDS`).

- It imports cards mounted after the container starts. A card needs a `DCIM/` or `PRIVATE/` folder and must stay mounted for 10 s.
- Whatever was mounted at startup is never imported.
- It can't unmount the card or send Unraid notifications, so check the web UI.

## Updating

- Docker → Check for Updates → Apply Update. This restarts the container, so wait for running imports, promotes and archive scans first.
- Re-run the script `curl` above when the release notes say the script changed. `sd2nas.cfg` keeps your settings.
- Existing installs: edit the container → Advanced View → Extra Parameters: `--restart=unless-stopped`.
- Downgrading is not supported: an older SD2NAS refuses a newer catalog. To go back, stop the container and restore `catalog-YYYY-MM-DD.db` from `Management/SD2NAS` into appdata as `sd2nas.db`.

## Troubleshooting

- `grep sd2nas /var/log/syslog`: the script logs every result there.
- Open `/healthz`.
- A container that exits with code 3 found another SD2NAS using the same appdata folder. The log line names it:
  `/config is in use by another SD2NAS (pid ... on ... since ...); stop it first`.
  With `--restart=unless-stopped`, Docker keeps restarting that container and the line repeats. That is harmless, but stop the duplicate container.
- An import can be started by hand:

  ```
  curl -s -X POST -H 'X-SD2NAS: 1' -H 'Content-Type: application/json' 'http://localhost:<port>/api/ingest?wait=1&format=text' -d '{"path":"/cards/<folder under /mnt/disks>"}'
  ```

## Development

See [docs/PLAN.md](docs/PLAN.md) section 14. In short:

```
go run ./cmd/sd2nas --cards ./testdata/cards --photos ./tmp/photos --config ./tmp/config
```

and `npm run dev` in `web/`.
