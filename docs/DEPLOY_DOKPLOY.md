# Deploy ke Dokploy (project "Ritel35k - ko Aci")

Repo: `divinecoid/ritel35k-backend` dan `divinecoid/ritel35k-frontend` (branch `main`). Empat service dalam satu project
Dokploy, satu jaringan internal. **Hanya frontend yang diberi domain publik** — browser tidak pernah bicara langsung ke API
(frontend memproksi `/api/*` dan menyimpan token di cookie HttpOnly).

Redis tidak dipakai kode saat ini (`REDIS_URL` boleh dikosongkan); MinIO juga belum dipakai.

## 1. PostgreSQL (service "Database" Dokploy)
- Image `postgres:17`. Database `ritel35k`, user `ritel35k`, password kuat (catat).
- Ekstensi `ltree` dan `citext` ikut di image resmi (dibuat oleh migrasi).
- Aktifkan volume persisten + backup.
- Nama host internal = nama service/app di Dokploy (mis. `ritel35k-db`), port 5432.

## 2. Backend API (Application → Git → `ritel35k-backend`, branch `main`)
- Build type: **Dockerfile**, path `Dockerfile`, context `.`
- Port container: `8080`. Tanpa domain publik.
- Health check: `GET /ready` (200 bila DB terjangkau).
- Environment:

| Variabel | Nilai |
|---|---|
| `APP_ENV` | `prod` |
| `HTTP_ADDR` | `:8080` |
| `DATABASE_URL` | `postgres://ritel35k:<PASSWORD>@<host-db>:5432/ritel35k?sslmode=disable` |
| `JWT_SECRET` | acak ≥ 32 karakter, mis. `openssl rand -hex 32` (jangan diganti setelah ada pengguna: semua sesi putus) |
| `CORS_ORIGINS` | `https://<domain-frontend>` |
| `TRUST_PROXY` | `true` |
| `ACCESS_TTL` / `REFRESH_TTL` | `15m` / `720h` (default) |
| `LOG_LEVEL` | `info` |

## 3. Worker (Application kedua dari repo & Dockerfile yang sama)
- Override **Entrypoint/Command**: `/app/worker`
- Environment: **sama persis** dengan API (kecuali port). Tanpa domain, tanpa health check HTTP.
- Wajib jalan: relay outbox, rebuild ringkasan dashboard (tanpa worker dashboard kosong), notifikasi, job poin.
- Jalankan **satu** instance.

## 3b. Migrasi & perusahaan pertama (sekali, lalu setiap rilis dengan migrasi baru)
Dari terminal container API di Dokploy (atau Command sekali jalan dengan environment API yang sama):

```bash
/app/migration up                         # skema + partisi 3 bulan + sinkron permission
/app/migration bootstrap -code ACME -name "Nama Perusahaan" \
  -email owner@perusahaan.id -owner "Nama Owner" -password '<password-kuat>'
```
Migrasi harus dijalankan **sebelum** API versi baru menerima trafik (migrasi aditif, tetapi kode baru mengandalkannya).
Data demo (`backend/scripts/seed_demo.py`) hanya untuk dev; jangan dijalankan di server produksi.

## 4. Frontend (Application → Git → `ritel35k-frontend`, branch `main`)
- Build type: **Dockerfile**, path `Dockerfile`. Port container: `3000`.
- Domain publik + HTTPS (Let's Encrypt) di Dokploy → Domains.
- Environment:

| Variabel | Nilai | Catatan |
|---|---|---|
| `API_URL` | `http://<host-service-backend>:8080` | internal, tanpa `/api/v1` |
| `PROTOCOL_HEADER` | `x-forwarded-proto` | **wajib** |
| `HOST_HEADER` | `x-forwarded-host` | **wajib** |
| `PORT` | `3000` | sudah default di image |

Tanpa `PROTOCOL_HEADER`/`HOST_HEADER`, SvelteKit 3 menganggap origin `https://<host-internal>` dan **semua form POST
(login) ditolak 403 "Cross-site POST form submissions are forbidden"**. Env `ORIGIN` tidak berpengaruh di versi ini.
Traefik (Dokploy) otomatis mengirim `X-Forwarded-Proto` dan `X-Forwarded-Host`.

## 5. Urutan & verifikasi
1. DB → API (deploy) → `migration up` + `bootstrap` → Worker → Frontend.
2. `https://<domain>/ping` → `{"ok":true}` (frontend → API `/ready`).
3. Login dengan owner dari langkah bootstrap; buat toko di Organisasi → Toko; terminal `POS-01` otomatis ada.
4. Dashboard terisi setelah ada penjualan **dan** worker berjalan.

## 6. POS offline (agent toko, bukan di server)
Di komputer kasir tiap toko jalankan `ritel35k-edge` (build dari `edge/`, butuh folder `backend/` di sebelahnya karena
`replace ../backend`). Config `config.json`:

```json
{
  "listen": "127.0.0.1:9100",
  "token": "<rahasia ≥16 karakter, juga dimasukkan di POS → Perangkat>",
  "allowed_origins": ["https://<domain-frontend>"],
  "backend_url": "https://<domain-backend-atau-domain-yang-memetakan-/api/v1>/api/v1",
  "data_dir": "/var/lib/ritel35k-edge",
  "pos": [{ "name": "kasir1", "token": "trm_…" }]
}
```
- Token `trm_…` dibuat di back office: Pengaturan → Terminal POS → **Token offline** (tampil sekali).
- **Agent memanggil API backend langsung** (`/api/v1/edge/*`, token perangkat), bukan lewat frontend. Karena itu API perlu
  alamat yang bisa dijangkau dari toko: beri domain publik terpisah pada service backend (mis. `api.<domain>`), atau
  tambahkan rute `/api/v1/edge/*` di proxy. Endpoint `/edge/*` hanya menerima token perangkat; endpoint lain tetap butuh login.
- Di POS → Perangkat isi URL `http://127.0.0.1:9100` dan token agent.

## 7. Keamanan
- Ganti password SSH server yang pernah dituliskan di chat; pakai SSH key.
- `JWT_SECRET`, password DB, dan password owner hanya di environment Dokploy, bukan di git.
- Batasi akses Postgres ke jaringan internal (jangan publish port 5432).

## 8. Server 156.67.219.209 (nginx di depan, bukan Traefik)
Di server ini port 80/443 dipegang **nginx** milik host, bukan Traefik Dokploy. Pola yang dipakai semua app: publish port host
dari Dokploy (31xx/80xx) lalu nginx memproksi domain ke port itu. Hasil deploy RITEL35K:

| Service | Port host | Domain |
|---|---|---|
| ritel35k-web | 3130 → 3000 | `ritel35k.divine.co.id` |
| ritel35k-api | 3131 → 8080 | `ritel35k-be.divine.co.id` (butuh A record → 156.67.219.209) |
| ritel35k-worker | – | – |
| ritel35k-db (postgres:17) | tidak dipublish | – |

Contoh vhost nginx (frontend; `X-Forwarded-*` **wajib** agar login tidak 403):

```nginx
server {
    server_name ritel35k.divine.co.id;
    location / {
        proxy_pass http://127.0.0.1:3130;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_http_version 1.1;
        proxy_buffering off;          # SSE (live dashboard)
        proxy_read_timeout 1h;
    }
    listen 443 ssl; # certbot --nginx -d ritel35k.divine.co.id
}
```
Vhost API sama, `proxy_pass http://127.0.0.1:3131;` (tanpa kebutuhan `X-Forwarded-Host`; `X-Forwarded-For` tetap dikirim).
Setelah TLS aktif: `sudo certbot --nginx -d ritel35k.divine.co.id -d ritel35k-be.divine.co.id`.

Cara cepat: jalankan skrip yang sudah menulis kedua vhost, menguji `nginx -t` (rollback bila gagal), me-reload nginx, dan
meminta sertifikat:

```bash
curl -fsSL https://raw.githubusercontent.com/divinecoid/ritel35k-backend/main/deploy/setup-nginx.sh -o setup-nginx.sh
less setup-nginx.sh            # baca dulu
sudo bash setup-nginx.sh
```
