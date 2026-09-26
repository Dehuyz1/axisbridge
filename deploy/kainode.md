# Deploy ke VPS Kainode

Panduan singkat untuk deploy stack `axisbridge` split di VPS Kainode
(Ubuntu 22.04 / 24.04, Docker + docker compose plugin).

## 1. Siapkan VPS

```bash
# ssh masuk ke VPS
ssh root@<ip-kainode>

# install docker (skip kalau sudah ada)
curl -fsSL https://get.docker.com | sh
systemctl enable --now docker

# firewall minimal (kalau pakai ufw)
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw enable
```

Port `1213` (core) dan `5432` (postgres) sengaja **tidak** dibuka publik;
mereka bind ke `127.0.0.1` di compose. Yang publik hanya UI (`80`/`443`
lewat Caddy, atau `8080` langsung kalau tanpa Caddy).

## 2. Clone + config

```bash
cd /opt
git clone <repo-url> axisbridge
cd axisbridge

cp .env.example .env
nano .env
```

Wajib set di `.env`:

```
POSTGRES_PASSWORD=<random 32 char>
UI_BIND=127.0.0.1              # kalau pakai Caddy
CADDY_DOMAIN=axis.example.com  # kalau pakai Caddy
CADDY_EMAIL=you@example.com
```

Catatan: UI tidak punya autentikasi bawaan. Pastikan `UI_BIND=127.0.0.1`
dan gunakan Caddy/Nginx kalau UI diekspos ke internet.

Generate secret cepat:

```bash
openssl rand -base64 24 # untuk POSTGRES_PASSWORD
```

## 3. Bring up

### Mode A: langsung expose UI di port 5002 (tanpa HTTPS)

```bash
# set UI_BIND=0.0.0.0 di .env
docker compose up -d --build
# akses: http://<ip-vps>:5002  (tanpa auth — pastikan firewall atau reverse proxy melindungi)
```

### Mode B: Caddy + HTTPS otomatis (recommended)

```bash
# set UI_BIND=127.0.0.1 di .env
# arahkan DNS A record CADDY_DOMAIN ke ip VPS
docker compose -f docker-compose.yml -f docker-compose.caddy.yml up -d --build
# akses: https://axis.example.com  (Let's Encrypt jalan otomatis)
```

## 4. Cek

```bash
docker compose ps
docker compose logs -f axis-core
curl -s http://127.0.0.1:5001/healthz
```

## 5. Update

```bash
cd /opt/axisbridge
git pull
docker compose up -d --build
```

Migrasi berjalan otomatis saat `axis-core` boot (goose embedded).

## 6. Backup Postgres

```bash
docker compose exec -T postgres pg_dump -U axis axisbridge \
  | gzip > /opt/backups/axisbridge-$(date +%F).sql.gz
```

Cron harian:

```bash
0 3 * * * cd /opt/axisbridge && docker compose exec -T postgres pg_dump -U axis axisbridge | gzip > /opt/backups/axisbridge-$(date +\%F).sql.gz
```

## 7. Troubleshooting Kainode

- Kainode kadang kasih **kernel lama**; kalau `docker` gagal, upgrade
  kernel dulu atau minta template Ubuntu terbaru dari panel Kainode.
- Kalau VPS pakai **IPv6 only**, tambahkan resolver fallback di
  `/etc/docker/daemon.json`:
  ```json
  { "dns": ["1.1.1.1", "8.8.8.8"] }
  ```
  lalu `systemctl restart docker`.
- **Disk kecil (< 20GB)**: images total ~500MB, Postgres data tumbuh
  sesuai jumlah nomor + events. Bersihkan tabel `events` bulanan lewat
  cron kalau perlu.
- **RAM 1GB**: kecilkan `AXIS_WORKER_CONCURRENCY=2` dan `OVO_WORKER_CONCURRENCY=1`.

## Mode C: systemd native (Kainode production saat ini)

Di Kainode, binary Go berjalan langsung sebagai systemd unit — bukan
container. Postgres satu-satunya yang jalan sebagai container
(`axisbridge-pg` di `127.0.0.1:5000`). Layout:

- Source + binary: `/opt/axisbridge/`
- Binary: `/opt/axisbridge/bin/{axis-core,register-worker,gift-worker,axis-ui}`
- Env: `/opt/axisbridge/.env` dibaca via `EnvironmentFile=` di setiap unit
- Units: `axis-core`, `axis-register-worker`, `axis-gift-worker`, `axis-ui`

**Go di VPS ini 1.22; toolchain download diblokir — jangan naikkan `go` directive
di `go.mod` melebihi 1.22.**

### Deploy / update

```bash
ssh kainode "bash -lc '
  cd /opt/axisbridge && git pull
  GOPROXY=https://proxy.golang.org,direct go build -trimpath -ldflags=\"-s -w\" \
    -o bin/axis-core ./cmd/axis-core
  systemctl restart axis-core
'"
```

Rebuild satu binary (contoh `axis-ui`):

```bash
ssh kainode "bash -lc 'cd /opt/axisbridge && \
  GOPROXY=https://proxy.golang.org,direct go build -trimpath -ldflags=\"-s -w\" \
  -o bin/axis-ui ./cmd/axis-ui && systemctl restart axis-ui'"
```

Restart semua sekaligus:

```bash
ssh kainode "bash -lc 'systemctl restart axis-core axis-register-worker axis-gift-worker axis-ui'"
```

### Cek status

```bash
ssh kainode "bash -lc 'systemctl is-active axis-core axis-register-worker axis-gift-worker axis-ui'"
ssh kainode "bash -lc 'curl -sS http://127.0.0.1:5001/healthz'"
ssh kainode "bash -lc 'curl -sS http://127.0.0.1:5002/api/stats'"
```

### Postgres container

```bash
# status
ssh kainode "bash -lc 'docker ps --filter name=axisbridge-pg'"
# backup manual
ssh kainode "bash -lc 'docker exec axisbridge-pg pg_dump -U axis axisbridge \
  | gzip > /opt/backups/axisbridge-\$(date +%F).sql.gz'"
```
