# axisbridge (split)

Split rewrite of the monolithic `axisbridgev2`. Postgres-backed queue
(`LISTEN/NOTIFY` + `SKIP LOCKED`), 4 independent Go binaries, 4 dashboards.

```
┌───────────────────────┐         LISTEN/NOTIFY
│ PostgreSQL 16         │◀────────────────────────┐
│ accounts, jobs,       │                         │
│ settings              │                         │
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
┌─────────────────┐    ┌──────────────┐           │
│ register-worker │    │ gift-worker  │───────────┘
│ axis_otp_request│    │ axis_gift    │
└─────────────────┘    └──────────────┘
```

## Bring up (local dev)

```bash
cp .env.example .env
# set POSTGRES_PASSWORD in .env
docker compose up -d --build
# UI      http://localhost:5002     (no auth — front with reverse proxy if public)
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
  axis-ui/           4 dashboard (Management / Gift List / OVO / Setting)
internal/
  db/                pgx pool + embedded goose migrations
  jobq/              LISTEN/NOTIFY + SKIP LOCKED job runner
  settings/          live-reload settings snapshot
  api/               REST handlers untuk axis-core
  axis/              phone normalizer (client AXIS asli belum di-port)
  ovo/               OVO client: OTP login/register, saldo, PIN vault
  renew/             scanner yang enqueue axis_renew untuk nomor lewat masa
```

## Config

Two layers:

- **Bootstrap** (`.env`): `DATABASE_URL`, ports, `POSTGRES_PASSWORD`,
  `OVO_MASTER_KEY`. Wajib ada sebelum Postgres reachable.
- **Runtime** (`settings` table, editable dari `/setting` UI):
  `otp.max_attempt`, `otp.retry_gap`, `otp.debounce`, `gift.quota_daily`,
  `worker.register_enabled`, `worker.gift_enabled`, `renew.enabled`,
  `renew.minus_days`, `log.level`.
  Worker polling settings tiap 10s — perubahan langsung apply tanpa
  restart binary.

## Dashboards

- `/management` — daftar nomor (bukan `gift_only`). Kolom: Status · Nomor ·
  Masa Aktif · Paket — semua sortable + filterable. Status tampil sebagai
  `MATI` / `TIDAK MATI` dari boolean `dead`. Double-click badge status →
  dialog token (`axis_token` + `axis_refresh`). Aksi: register, resend OTP,
  pause, resume, delete, Upload List, Export CSV.
- `/gifts` — daftar gift sukses dari `gift-worker`. 8 kolom: Nomor, Paket,
  Nominal, Trx ID, OVO Nomor, OVO Trx, OVO Bayar, Waktu — plus checkbox +
  kolom Aksi. Delete per baris, delete terpilih, delete semua (confirm).
  Upload dari halaman ini **skip OTP**: nomor langsung `gift_only=true`,
  `status=gift_wait`, enqueue `axis_gift`. Nomor `gift_only` tidak muncul
  di `/management`.
- `/ovo` — wallet OVO untuk membayar renew & gift. Kolom: status (titik
  warna) · Nomor · Nama · Saldo · Saldo diambil · Token · Keterangan · Aksi.
  Tambah wallet menyimpan PIN terenkripsi; login terpisah lewat OTP WA / SMS,
  lalu kode diverifikasi di baris yang sama. OVO sendiri yang menentukan
  nomor itu login atau perlu register, dan UI menampilkan keputusan itu.
  Titik: hijau `active` (siap bayar), kuning `otp_sent`, merah
  `login_needed`, abu `new`/`disabled`.
- `/setting` — form group OTP / Gift / Worker / Renew, inline save ke Postgres.

## Register + gift flow

**Jalur A — register via `/management`:**

1. User register nomor di `/management`.
2. `axis-core` insert `accounts` (status=`otp_pending`) + `jobs` kind
   `axis_otp_request` (attempt=1).
3. `register-worker` klaim job → panggil AXIS RequestOTP (stub),
   naikkan `otp_attempts`, dan:
   - kalau `otp_attempts < otp.max_attempt` → enqueue attempt berikutnya
     dengan `scheduled_at = NOW() + otp.retry_gap`.
   - kalau `otp_attempts >= otp.max_attempt` → set `status=gift_wait` +
     insert `jobs` kind `axis_gift`.
4. `gift-worker` klaim job `axis_gift` → panggil AXIS gift (stub) →
   insert `gifts(msisdn, proof)` + set `status=gift_done`.

**Jalur B — upload langsung via `/gifts` (skip OTP):**

1. User upload list nomor dari halaman `/gifts` (`POST /api/gifts/bulk`).
2. `axis-core` insert/reuse `accounts` dengan `gift_only=true`,
   `status=gift_wait`, langsung enqueue `axis_gift`. OTP loop tidak
   disentuh sama sekali.
3. `gift-worker` memproses sama seperti langkah 4 di atas.

SMS balasan (OTP asli) yang bisa mengubah status jadi `available` datang
lewat endpoint `POST /api/provider/sms` yang akan ditambahkan ketika
client AXIS asli benar-benar di-port.

## REST endpoints

```
GET  /healthz

GET  /api/accounts                   list accounts (sort/filter via query)
POST /api/register                   register satu nomor
POST /api/accounts/bulk              register banyak nomor
GET  /api/accounts/{id}/token        reveal axis_token + axis_refresh
POST /api/accounts/{id}/resend       paksa ulang OTP loop
POST /api/accounts/{id}/pause        set status=paused
POST /api/accounts/{id}/resume       set status=new
DELETE /api/accounts/{id}            hapus account
GET  /api/accounts.csv               export CSV seluruh kolom

GET  /api/gifts                      list gift sukses
POST /api/gifts/bulk                 upload nomor skip-OTP (gift_only)
DELETE /api/gifts/{id}               hapus satu gift
POST /api/gifts/delete_all           hapus semua gift (body: {"confirm":true})
GET  /api/gifts.csv                  export CSV gifts

GET  /api/settings                   baca semua runtime settings
PUT  /api/settings/{key}             update satu setting

GET  /api/stats                      ringkasan: accounts, gifts, jobs
```

## Job queue mechanics

- `axis-core` insert row `jobs`. Trigger `notify_new_job` → `NOTIFY
  jobs_<kind>, '<id>'`.
- Setiap worker `LISTEN` hanya kind yang dia handle → wake instan.
- Fallback poll 5s jaga-jaga NOTIFY hilang.
- Klaim: `SELECT … FOR UPDATE SKIP LOCKED LIMIT 1` — aman multi-replica.
- Panic-safe: handler yang panic dianggap fail.

## Renew pipeline

- `internal/renew` scanner jalan di dalam `axis-core` (bukan binary sendiri:
  kerjanya hanya `INSERT … SELECT` ke DB yang sudah dimiliki axis-core).
- Tiap tick (`renew.scan_every`, minimum 1m) ia cari nomor yang masa aktifnya
  sudah lewat `renew.minus_days` hari, lalu enqueue satu `axis_renew`.
- Dikecualikan: `dead`, `gift_only`, `paused`, dan nomor yang baru di-charge
  dalam `renew.cooldown`.
- Unique index `idx_jobs_active_per_account` menjamin satu nomor tidak pernah
  punya dua `axis_renew` aktif, jadi tick berulang aman (idempoten).
- `renew.enabled=false` mematikan enqueue sepenuhnya.

## Next milestones

1. Port AXIS client asli (`axisbridgev2/axis`) → `internal/axis` +
   pasang di `register-worker` (`RequestOTP`) dan `gift-worker` (gift
   flow).
2. Tambah endpoint `POST /api/provider/numbers` + `POST /api/provider/sms`
   di `internal/api` supaya ModemGo bisa push nomor & OTP.
3. `pay-worker`: konsumsi `axis_renew` → charge AXIS → `ScanQR` → `PayNotif`
   pakai wallet dari `/ovo`, tulis hasilnya ke tabel `transactions`.
