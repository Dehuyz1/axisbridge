# axisbridge (split)

Split rewrite of the monolithic `axisbridgev2`. Postgres-backed queue
(`LISTEN/NOTIFY` + `SKIP LOCKED`), 4 independent Go binaries, 2 dashboards.

```
┌───────────────────────┐         LISTEN/NOTIFY
│ PostgreSQL 16         │◀────────────────────────┐
│ accounts, jobs,       │                         │
│ settings, ovo_accounts│                         │
└─────┬─────────────────┘                         │
      │ SQL                                       │
      ▼                                           │
┌──────────────┐     REST      ┌──────────────┐   │
│  axis-core   │◀──────────────│   axis-ui    │   │
│  :1213 API   │               │  :8080 UI    │   │
└──────────────┘               └──────────────┘   │
      ▲                                           │
      │ INSERT jobs                               │
      │                                           │
      ▼                                           │
┌──────────────┐    ┌───────────────┐             │
│ axis-worker  │    │  ovo-payer    │─────────────┘
│ AXIS client  │    │  OVO client   │
└──────────────┘    └───────────────┘
```

## Bring up (local dev)

```bash
cp .env.example .env
# set UI_PASS + OVO_MASTER_KEY
docker compose up -d --build
# UI      http://localhost:5002     (basic auth: UI_USER / UI_PASS)
# core    http://localhost:5001/healthz
```

## Deploy VPS (Kainode)

Panduan lengkap: [`deploy/kainode.md`](deploy/kainode.md).

Ringkas:

```bash
# di VPS
curl -fsSL https://get.docker.com | sh
cd /opt && git clone <repo-url> axisbridge && cd axisbridge
cp .env.example .env && nano .env       # set secrets + UI_BIND=127.0.0.1
docker compose -f docker-compose.yml -f docker-compose.caddy.yml up -d --build
# HTTPS otomatis via Caddy + Let's Encrypt di CADDY_DOMAIN
```

Postgres dan axis-core selalu bind ke `127.0.0.1` — hanya UI (via Caddy)
yang tembus internet.

## Layout

```
cmd/
  axis-core/         REST API + migration owner
  register-worker/   consume axis_otp_request; loop 3x, lalu → gift_wait
  gift-worker/       consume axis_gift; sukses → row baru di tabel gifts
  axis-ui/           3 dashboard (Management / Gift List / Setting)
internal/
  db/                pgx pool + embedded goose migrations
  jobq/              LISTEN/NOTIFY + SKIP LOCKED job runner
  settings/          live-reload settings snapshot
  api/               REST handlers untuk axis-core
  axis/              phone normalizer (client AXIS asli belum di-port)
```

## Config

Two layers:

- **Bootstrap** (`.env`): `DATABASE_URL`, ports, `UI_PASS`. Wajib ada
  sebelum Postgres reachable.
- **Runtime** (`settings` table, editable dari `/setting` UI):
  `otp.max_attempt`, `otp.retry_gap`, `otp.debounce`, `gift.quota_daily`,
  `worker.register_enabled`, `worker.gift_enabled`, `log.level`.
  Worker polling settings tiap 10s — perubahan langsung apply tanpa
  restart binary.

## Dashboards

- `/management` — daftar semua nomor. Kolom: ID, Nomor, Status, OTP
  Attempts, Last OTP, Last Login. Aksi: register, resend OTP, pause,
  delete. Footer live: counter per status + total gift.
- `/gifts` — **cuma** 2 kolom fungsional: Nomor + Bukti Transaksi Sukses.
  Setiap baris = satu gift sukses yang di-insert oleh `gift-worker`.
- `/setting` — form group (OTP / Gift / Worker), inline save ke Postgres.

## Register + gift flow

1. User register nomor di `/management`.
2. `axis-core` insert `accounts` (status=`otp_pending`) + `jobs` kind
   `axis_otp_request` (attempt=1).
3. `register-worker` klaim job → panggil AXIS RequestOTP (stub sekarang),
   naikkan `otp_attempts`, dan:
   - kalau `otp_attempts < otp.max_attempt` → enqueue attempt berikutnya
     dengan `scheduled_at = NOW() + otp.retry_gap`.
   - kalau `otp_attempts >= otp.max_attempt` → set `status=gift_wait` +
     insert `jobs` kind `axis_gift`.
4. `gift-worker` klaim job `axis_gift` → panggil AXIS gift (stub) →
   insert `gifts(msisdn, proof)` + set `status=gift_done`.

SMS balasan (OTP asli) yang bisa mengubah status jadi `available` datang
lewat endpoint `POST /api/provider/sms` yang akan ditambahkan ketika
client AXIS asli benar-benar di-port.

## Job queue mechanics

- `axis-core` insert row `jobs`. Trigger `notify_new_job` → `NOTIFY
  jobs_<kind>, '<id>'`.
- Setiap worker `LISTEN` hanya kind yang dia handle → wake instan.
- Fallback poll 5s jaga-jaga NOTIFY hilang.
- Klaim: `SELECT … FOR UPDATE SKIP LOCKED LIMIT 1` — aman multi-replica.
- Panic-safe: handler yang panic dianggap fail.

## Next milestones

1. Port AXIS client asli (`axisbridgev2/axis`) → `internal/axis` +
   pasang di `register-worker` (`RequestOTP`) dan `gift-worker` (gift
   flow).
2. Tambah endpoint `POST /api/provider/numbers` + `POST /api/provider/sms`
   di `internal/api` supaya ModemGo bisa push nomor & OTP.
3. Feature terpisah OVO/renewal (belum dipasang lagi; keluar dari scope
   sekarang biar bug OVO tidak nyangkut ke register/gift).
