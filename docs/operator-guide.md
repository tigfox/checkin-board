# checkin-board operator guide

For the person setting up and running a checkpoint or HQ node. Each
node is a graywolf APRS station (often a Pi Zero W) with checkin-board
installed beside it. Volunteers use their phones on the node's Wi-Fi.

- **Checkpoint** nodes log runners' bibs and send them to HQ by radio.
- The **HQ** node (net control) receives every checkpoint's reports and
  shows the status board.

## 1. Install

You need a node with graywolf already running, and shell access.

1. Copy the release bundle to the node and unpack it in your home directory (not `/tmp`: on Raspberry Pi OS it lives in RAM, and a Pi Zero has little to spare):
   ```sh
   tar xzf checkin-board-<version>-linux.tar.gz
   cd checkin-board-<version>
   sudo ./install.sh
   ```
   The script:
   - creates a `checkin-board` system user;
   - installs the binary to `/usr/local/bin`;
   - writes `/etc/checkin-board/checkin-board.env`;
   - creates the app's own graywolf login, `checkin-board`, with a
     random password, and saves the password in
     `/etc/checkin-board/gw-password` (mode 600). This is a login
     inside graywolf, not a Linux user. graywolf's web UI can't add
     users, so the script uses graywolf's command, run as the owner of
     graywolf's database (`/var/lib/graywolf/graywolf.db`; set
     `GRAYWOLF_DB` if yours is elsewhere). If it can't, it asks for a
     password instead (see "The app's graywolf login" below);
   - starts the `checkin-board` service. If you skip the password (or
     run the script non-interactively), it says "Not started": put the
     password in that file and `sudo systemctl start checkin-board`.
2. If graywolf isn't on `http://127.0.0.1:8080`, edit
   `/etc/checkin-board/checkin-board.env` and
   `sudo systemctl restart checkin-board`.

Data lives in `/var/lib/checkin-board/`:

- `checkin-board.db`: the database.
- `race-journal.csv`: the power-safe bib journal.
- `backups/`: written on every reset and upgrade, never deleted by the
  app.

**Upgrading:** run `sudo ./install.sh` from the new bundle. Settings and
the password are kept, and the database is copied to
`backups/pre-upgrade-<time>/` before the new version starts. If the
upgrade fails, the previous version is started again. Backups are never
pruned: delete old ones yourself when the SD card fills up.

**The app's graywolf login.** The app uses it to send and read
messages, read the packet log, and read the station callsign (or change
it, from Admin → Station). graywolf logins have no permission levels, so
this one can do anything graywolf's admin can. Keeping it separate lets
you change or revoke the app's password without touching yours. The
script only ever creates or resets the login named `checkin-board`, and
only while no password is saved. To create it by hand (it asks for a
password of 8+ characters, without spaces):

```sh
sudo -u graywolf graywolf auth set-password --user checkin-board -config /var/lib/graywolf/graywolf.db
```

Then put the same password in `/etc/checkin-board/gw-password`. To use
your own graywolf login instead, set `GW_USER` in
`/etc/checkin-board/checkin-board.env` before running the script, and
enter its password when asked.

The app listens on port 8090 on every interface (`CB_LISTEN` in the env
file). If the node also has an uplink you don't want it on, set
`CB_LISTEN` to the hotspot address, e.g. `CB_LISTEN=192.168.4.1:8090`.

## 2. First-run setup (each node)

1. Get the one-time setup code:
   `journalctl -u checkin-board | grep setup_code`
2. Open `http://<node>:8090/` and enter the code and a new **admin**
   password (10+ characters). Keep it with the station operator.
3. **Admin → Passwords:** set the **volunteer** password (6+ characters).
   This is the one password everyone on the keypad shares.
4. **Admin → Station:**
   - **Callsign (graywolf):** this changes graywolf's station callsign
     for all of graywolf, not just the race. It asks you to confirm.
   - **Role:** Checkpoint or HQ. The role can only be changed before the
     race starts.
   - **Race name** and **station tactical name** (e.g. `AID3`).
   - Checkpoint only: the **checkpoint code** (e.g. `AS5`) and **HQ
     callsign**.
   - HQ only: **local codes**, the codes HQ itself logs on its own keypad
     (e.g. `START,FIN`).
   - Leave the timing settings at their defaults unless told otherwise.
   - Check the **graywolf connection** panel. It should say reachable,
     with live updates on.

### HQ extra setup

Use **Admin → HQ** for these.

- **Checkpoints:** add every checkpoint in course order with its code,
  its name and the callsign it transmits from. Once the list exists, HQ
  only asks listed checkpoints for missing reports. A checkpoint heard
  from an unexpected callsign is flagged.
- **Roster** (optional): import a CSV with the bib in the first column.
  Only a column headed `category` is kept. Names and other personal data
  are discarded.
- **Admin → Branding:** set the status board's header, footer, colours
  and logo. The editor refuses colour pairs that are hard to read.

### Link check (each checkpoint, at its location)

Before the race, once the checkpoint is set up where it will operate:
**Admin → Link check → Run link check**. It sends 5 short test messages
to HQ, 10 s apart. HQ's app answers them automatically, and after one to
three minutes you get a verdict:

- **PASS:** the link is good.
- **MARGINAL:** usable, but expect retries and delays. Try a higher
  antenna, a relay station or a digipeater path, then check again. On a
  doubtful link, 10 probes give a steadier verdict than 5.
- **FAIL:** not usable as it is. Check the frequency, the channel, the
  HQ callsign and the antenna.

The audio levels shown are the sound-card input level, not signal
strength. "Too hot" or "very low" means adjust the radio's volume or the
input gain. Runs are at least 2 minutes apart. HQ can also check any
checkpoint from its own Link check tab, and its health panel shows each
checkpoint's latest result. **Start race** warns about any link without
a PASS in the last 2 hours. The warning doesn't block the start.

### The node panel (e-ink display)

If the node has the e-ink bonnet and SPI is on, `install.sh` starts the
panel service. To turn SPI on: `sudo raspi-config nonint do_spi 0`,
reboot, and run `install.sh` again. The panel shows the node's status and
the address volunteers should open, refreshing every few minutes.

- **First start:** bonnets come in several revisions, so the panel finds
  its own. It shows "Readable? Then press a button." with each candidate
  in turn, about 20 s apart. Press either button when the text is clear.
  If nothing is pressed after three rounds, it stops. Choose the
  controller, or **Detect again**, on Admin → Panel.
- **Buttons:**
  - Any press opens the menu.
  - **Top** moves to the next item; **bottom** selects.
  - Actions that change the race, and link checks, ask again: bottom to
    confirm, top to cancel.
  - After 30 s untouched, the panel returns to the status screen.
- **The menu** is edited on **Admin → Panel**: which items, their names
  and order. Reset, cleanup and passwords are never on the panel.
- **The display rests 3 minutes between full refreshes**, to protect it.
  That's why the status can lag a change by a few minutes.

## 3. Race day

| When | Checkpoint admin | HQ admin |
|------|------------------|----------|
| Before the first runner | **Start race**. Check the keypad's clock banner: if it says the clock isn't set, tap **Set time from this device** on a phone with the right time | **Start race**. Open the **status board** |
| During | Watch "N unconfirmed, last HQ contact …" on the keypad. Entries keep, and keep sending, if the link drops | Watch **Checkpoint health**: quiet checkpoints, missing batches, clock skew, sender mismatch |
| Last runner through | **Complete race**: the keypad closes (voids still work); everything queued is sent at once | **Complete race** when every checkpoint is in |
| Packing up | **Secure for travel**: the radio goes quiet; the page shows how many entries will go out at check-in | — |
| Back at HQ | **Final check-in**: everything HQ hasn't confirmed is sent at once. The node moves to **Checked in** by itself when HQ has confirmed everything | Health shows the checkpoint heard; missing batches are requested automatically |

Volunteers: log in at `http://<node>:8090/` with the volunteer
password, type the bib and tap **LOG**. If a save fails, the digits stay
on screen. Tap **LOG** again: a retry never logs the bib twice. **Void**
fixes a mistake.

## 4. When something goes wrong

**A checkpoint can't reach HQ.** Nothing is lost. Entries stay queued
and keep retrying with back-off. Final check-in at HQ sends the rest. If
the radio can't finish even then:

- At the checkpoint, use **Admin → Outbox → Download checkpoint export**.
- Carry the file to HQ and use **Admin → HQ → Import checkpoint export**.
  Importing is exact and safe to repeat, alongside radio delivery.

**HQ shows missing batches from a checkpoint.** HQ asks for them
automatically. **Re-request** on the health row asks again now.

**A checkpoint's database is lost or corrupt.** The bib journal
(`/var/lib/checkin-board/race-journal.csv`) is written before the
database, so it survives power cuts. At HQ, use **Admin → HQ → Import
journal** with that file.

**HQ's database is lost.** Restore a backup, or start fresh and use
**Admin → Station → Re-read graywolf messages** from the race start
time. graywolf still holds the race messages until cleanup.

**The keypad says "saved in the backup journal only".** The database
couldn't be written, for example because the disk is full. The bib is
safe in the journal. Don't re-enter it; fix the disk and tell HQ.

**graywolf rejected the app's login** (keypad banner; "LOGIN FAILED"
on Admin → Station). Check the password in
`/etc/checkin-board/gw-password` matches the graywolf login, then
`sudo systemctl restart checkin-board`.

**Lost admin password.** On the node (the password isn't echoed or kept
in shell history):
```sh
read -rs P && printf '%s\n' "$P" | sudo -u checkin-board CB_DB_PATH=/var/lib/checkin-board/checkin-board.db checkin-board reset-admin-password; unset P
```
Always run it as `-u checkin-board`: run as root, it can leave files the
service can't open. This logs every admin out and doesn't touch race data.

**Logs:** `journalctl -u checkin-board -f`. **Version:** `checkin-board version`.

## 5. After the race

1. **HQ:** use **Admin → HQ → Download results CSV**.
2. **graywolf cleanup** (optional; Admin → Race): deletes the app's
   race messages from graywolf. The operator's own conversations are
   never touched, and nothing HQ hasn't confirmed is deleted.
3. **Reset** (Admin → Race), to reuse the node:
   - Type the race name to confirm (`RESET` if the race has no name).
   - A backup goes to `/var/lib/checkin-board/backups/<race>-<time>/`
     first. It holds a database snapshot, the export or results, and the
     journal.
   - Settings are always kept. At HQ, branding, the checkpoint list and
     the roster are kept unless you tick "Also clear the checkpoint list
     and roster" or "Also reset the board branding".
   - A checkpoint with entries HQ hasn't confirmed refuses to reset
     unless you tick "Reset even if HQ hasn't confirmed everything". The
     entries are then only in the backup.
