# Remote link check through a graywolf Action (optional)

Net control can ask a checkpoint to run its link check (spec 4.8) by
radio, without anyone at the checkpoint touching the admin page:

```
@@<otp>#linkcheck        sent from HQ to the checkpoint's callsign
ok: PASS N0CALL-10 up5/5 ack5 rtt4s -21/-22dB      the checkpoint's reply
```

The reply reads: verdict, peer, probes heard at HQ out of those sent,
probes ACKed, median round trip, then the receive audio level at HQ and
at the checkpoint (dBFS; `?` = no reading).

It uses graywolf's **webhook** Action calling checkin-board on the same
node. A command Action can't be used: graywolf's service runs with
`NoNewPrivileges`, so a script can't switch to the `checkin-board` user
that owns the app's database.

The hook stays off until you give it a token. Even then, it answers only
requests from the node itself that carry that token. It
starts no run during the race: during the race, run the check from the
admin page.

Don't put a reverse proxy (nginx, Caddy) on the same node in front of
the app. Every client would then appear to come from the node itself,
and only the token would protect the hook.

## 1. On the checkpoint node: the hook token

`install.sh` creates it (`/etc/checkin-board/hook-token`); the node panel
uses it too. Copy it for step 2:

```sh
sudo cat /etc/checkin-board/hook-token
```

The startup log line `starting config=… hook=on` confirms the hook is on.

## 2. In the checkpoint's graywolf: define the Action

The steps below use graywolf's own pages: see its handbook, "Actions".

1. **Actions → OTP credentials:** create one for net control, if you
   don't have one yet.
2. **+ New Action:**

   | Field | Value |
   |---|---|
   | Name | `LINKCHECK` |
   | Type | webhook |
   | URL | `http://127.0.0.1:8090/api/hook/linkcheck` (match `CB_LISTEN`) |
   | Method | POST |
   | Headers | `Authorization: Bearer <token from step 1>` and `Content-Type: application/json` |
   | Body template | `{}` |
   | Require OTP | on, with net control's credential |
   | Sender allowlist | HQ's callsign(s), e.g. `N0CALL-*` |
   | Timeout (sec) | 300 (a run takes one to three minutes) |
   | Queue depth | 1 |
   | Rate limit (sec) | 120 (the app also allows one run per 2 minutes) |
   | Max reply lines | 1 |

   The token is stored in graywolf's configuration. Anyone who can read
   that configuration, or reach the node's shell, can run link checks:
   the same people who can already transmit from the node.

3. Use graywolf's **Test** button on the Action. Its reply should be a
   result line like the one above, or a reason the run didn't start
   (e.g. `runs are 2 min apart; retry in 75s`).

## 3. From HQ

Send `@@<otp>#linkcheck` as a message to the checkpoint's callsign. The
checkpoint probes HQ (HQ's checkin-board answers automatically). After
one to three minutes the result comes back as graywolf's reply. HQ's
admin page then shows the run in its health panel and on its Link check
tab.

## From a script on the node

The same check from the node's shell, for deployment scripts:

```sh
sudo -u checkin-board CB_DB_PATH=/var/lib/checkin-board/checkin-board.db checkin-board linkcheck --brief
echo $?   # 0 PASS, 1 MARGINAL, 2 FAIL or couldn't run
```

`--json` prints the full result. `--count N` (up to 10) sends more
probes, which gives a steadier verdict on a doubtful link. `--yes` allows
a run during the race. On HQ, `--to CALL` picks the checkpoint to probe.
