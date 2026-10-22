# fc-gh-act-ctrl

GitHub Actions の self-hosted runner を **Firecracker microVM** 上で 1 ジョブ使い捨てで実行するコントローラです。

- GitHub App の `workflow_job` Webhook を受信
- ジョブごとに JIT runner (`generate-jitconfig`) を発行
- Firecracker microVM を起動し、MMDS 経由でゲストエージェントに JIT config を渡す
- ランナーは 1 ジョブだけ実行して終了し、VM は自動的に破棄される

設計の詳細は [docs/design.md](docs/design.md) を参照してください。

## 必要なもの

- Linux (x86_64 / aarch64) + KVM (`/dev/kvm`)
- root 権限 (TAP/bridge 操作、Firecracker 起動)
- Firecracker v1.x
- ゲストカーネル (`vmlinux`) と rootfs (`ext4`)
- GitHub App

ホストには次のパッケージが必要です (Debian/Ubuntu の例):

```bash
sudo apt-get install -y docker.io e2fsprogs iptables iproute2 curl
```

## クイックスタート

```bash
# 1. ビルド
make build            # bin/fcghact
make guest            # bin/fcghact-guest (rootfs に同梱するエージェント)

# 2. Firecracker とカーネル
sudo ./scripts/install-firecracker.sh
sudo ./scripts/download-kernel.sh

# 3. ホストネットワーク (bridge + NAT)
sudo ./scripts/setup-host.sh

# 4. rootfs のビルド (Docker が必要)
sudo ./scripts/build-rootfs.sh

# 5. 設定
sudo install -d /etc/fcghact
sudo cp configs/fcghact.example.yaml /etc/fcghact/fcghact.yaml
# GitHub App の設定画面からダウンロードした Private key を指定する
sudo install -m 600 /path/to/<app-name>.private-key.pem /etc/fcghact/app.pem
# Webhook secret は十分な長さの乱数を生成する
openssl rand -hex 32 | sudo tee /etc/fcghact/webhook_secret >/dev/null
# /etc/fcghact/fcghact.yaml の github.app_id を App の ID に変更する

# 6. 事前チェック
sudo bin/fcghact doctor --config /etc/fcghact/fcghact.yaml

# 7. 起動
sudo bin/fcghact serve --config /etc/fcghact/fcghact.yaml
```

systemd を使う場合:

```bash
sudo install -m 644 deploy/fcghact.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now fcghact
```

## GitHub App の設定

1. GitHub App を作成し、次の権限を付与する。
   - Repository permissions: **Administration: Read and write** (repo スコープの JIT config 発行)
   - Organization permissions: **Self-hosted runners: Read and write** (org スコープを使う場合)
2. Subscribe to events: **Workflow jobs** を有効にする。
3. Webhook URL を `https://<host>/webhook`、Secret を設定する。GitHub から到達できる必要があるため、必要ならリバースプロキシ (Caddy / nginx) で TLS 終端して公開する。
4. 対象リポジトリ (または組織) に App をインストールする。
5. App の設定画面 → **Private keys** → **Generate a private key** で `.pem` をダウンロードする。
   ホストへコピーして `github.private_key_path` に置く:

   ```bash
   # 例: ブラウザのダウンロード先からホストへ配置
   sudo install -d /etc/fcghact
   sudo install -m 600 ~/Downloads/<app-name>.private-key.pem /etc/fcghact/app.pem
   sudo openssl pkey -in /etc/fcghact/app.pem -noout -check
   ```

   `app.pem` はリポジトリには含まれません。`fcghact doctor` で
   `app private key` と `github app` が `ok` になることを確認してください。

6. `configs/fcghact.example.yaml` の `github.app_id` を App の **App ID** に変更する
   (プレースホルダの `123456` のままでは GitHub API が `404 Integration not found` を返します)。

`github.scope` は既定の `repo` を推奨します。`org` にすると 1 つのランナーグループで組織内の複数リポジトリを扱えますが、App の組織権限が必要です。

## rootfs の OCI 配布

rootfs は OCI イメージとして GHCR などに置けます。イメージは `rootfs.ext4` を 1 つ含む tar+gzip レイヤです。

```yaml
rootfs:
  # 起動時にこの参照を解決し、キャッシュした ext4 を使う
  oci_reference: ghcr.io/tuna2134-workspace/fcghact-rootfs:v1
  # 省略時は <firecracker.state_dir>/images
  cache_dir: /var/lib/fcghact/images
  # 省略時は docker login の資格情報 (root の場合は /root/.docker/config.json)
  token_env: GHCR_TOKEN
  prune: true
```

`rootfs.oci_reference` を設定すると、`firecracker.rootfs_image` はキャッシュされたファイルに置き換えられます。キャッシュは `<cache_dir>/sha256-<layer-digest>/rootfs.ext4` に置かれ、レイヤのダイジェストを検証してから atomic に rename します。

手動での操作:

```bash
# ビルドして push
sudo ./scripts/build-rootfs.sh
bin/fcghact rootfs push --file /var/lib/fcghact/rootfs.ext4 \
  --ref ghcr.io/OWNER/fcghact-rootfs:v1

# pull / 確認
bin/fcghact rootfs pull --ref ghcr.io/OWNER/fcghact-rootfs:v1
bin/fcghact rootfs inspect --ref ghcr.io/OWNER/fcghact-rootfs:v1
```

`ROOTFS_PUSH_REF=ghcr.io/OWNER/fcghact-rootfs:v1 sudo -E ./scripts/build-rootfs.sh` のようにすると、rootfs のビルドと push をまとめて実行できます。

[.github/workflows/rootfs.yml](.github/workflows/rootfs.yml) は `images/rootfs/**` や `guest/agent/**` の変更、または `rootfs-*` タグで rootfs をビルドし、GHCR に `12 桁 SHA` / `latest` / タグ名で push します (GHCR のパッケージ書き込み権限を使用)。

本番ではタグではなく **ダイジェスト固定** (`ghcr.io/...@sha256:...`) を推奨します。pull 時に image manifest とレイヤのダイジェストを検証します。

## 公開とアクセス制御

Webhook は GitHub から到達できる必要があります。インターネットに公開すると `/.env` や
`/.git/config` を探す自動スキャンが常時来ますが、404 を返すだけで影響はありません。
既定では **Webhook 以外のアクセスログは DEBUG レベル**で出力されるため、INFO ログが
スキャンで埋まることはありません (`log.level: debug` にすると確認できます)。

推奨:

- `server.listen` を `127.0.0.1:8080` にして、リバースプロキシで `/webhook` だけ公開する。
- `/metrics` と `/healthz` は外部公開しない。
- `server.webhook_path` を推測しにくいパスにする (ログノイズ軽減。認証は HMAC が担う)。

Caddy の例:

```caddyfile
example.com {
    handle /webhook {
        reverse_proxy 127.0.0.1:8080
    }
    respond 404
}
```

nginx の例:

```nginx
location = /webhook {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
}
location / { return 404; }
```

## ワークフロー側

```yaml
jobs:
  test:
    runs-on: [self-hosted, fcghact]
    steps:
      - uses: actions/checkout@v4
      - run: make test
```

`runner.require_labels` (既定 `fcghact`) を必ず含めてください。これがないと、他の self-hosted ランナー向けのジョブを誤って取得する可能性があります。

## コマンド

| コマンド | 説明 |
|---|---|
| `fcghact serve` | Webhook サーバとコントローラを起動 (既定) |
| `fcghact run --repo owner/name --job-id 123` | queued ジョブを 1 つ手動で実行 (デバッグ用) |
| `fcghact cleanup` | 残存 VM / TAP / state を回収 |
| `fcghact rootfs pull` | OCI イメージから rootfs を取得 (キャッシュ) |
| `fcghact rootfs push` | ext4 rootfs を OCI イメージとして push |
| `fcghact rootfs inspect` | マニフェストを解決して digest / キャッシュ状態を表示 |
| `fcghact doctor` | KVM・イメージ・bridge・GitHub 認証をチェック |
| `fcghact version` | バージョン表示 |

## 設定

`configs/fcghact.example.yaml` を参照。主な項目:

| キー | 既定値 | 説明 |
|---|---|---|
| `github.scope` | `repo` | JIT config を repo / org のどちらで発行するか |
| `github.repositories` | 空 | 許可する `owner/name` のリスト。空なら全インストール先 |
| `github.organizations` | 空 | 許可する組織名のリスト |
| `runner.labels` | `self-hosted,linux,x64,fcghact` | ランナーが広告するラベル |
| `runner.require_labels` | `fcghact` | ジョブが必ず含むべきラベル |
| `runner.max_concurrent_jobs` | `4` | 同時に起動する VM 数 |
| `runner.max_jobs_per_repo` | `2` | 1 リポジトリあたりの同時実行数 |
| `runner.job_timeout` | `60m` | 1 ジョブの上限時間 (超過で VM 強制停止) |
| `runner.registration_timeout` | `5m` | ランナーがジョブを取得しないまま経過したら停止 |
| `firecracker.mem_size_mib` | `2048` | VM あたりのメモリ |
| `firecracker.reflink` | `true` | rootfs を COW コピーする |
| `network.cidr` | `172.31.0.0/16` | ゲスト用サブネット |
| `rootfs.oci_reference` | 空 | rootfs を取得する OCI 参照。空なら `firecracker.rootfs_image` を使用 |
| `rootfs.cache_dir` | `<state_dir>/images` | rootfs のレイヤキャッシュ |
| `rootfs.token_env` | 空 | レジストリトークンを読む環境変数名 |
| `rootfs.prune` | `true` | 使用中以外のキャッシュレイヤを削除 |

環境変数は `FCGHACT_` プレフィックスで上書きできます (例: `FCGHACT_SERVER_LISTEN`, `FCGHACT_GITHUB_WEBHOOK_SECRET`)。

## 構成

```
cmd/fcghact          CLI / デーモン
internal/config      YAML + 環境変数
internal/githubapp   GitHub App 認証・JIT config・job 状態
internal/webhook     HMAC 検証と workflow_job のパース
internal/controller  ラベル照合・スケジューリング・監視
internal/vm          Firecracker 起動・rootfs COW・MMDS・孤児回収
internal/network     bridge / TAP / IPAM
internal/oci         rootfs の OCI push / pull / inspect
guest/agent          VM 内エージェント (MMDS → runner)
images/rootfs        ゲスト rootfs の Dockerfile
scripts              ホストセットアップ用スクリプト
```

## セキュリティ

- ジョブは microVM 内で root として動くが、ホストからは KVM で分離される。rootfs はジョブごとに COW コピーされ、終了時に削除される。
- JIT config は MMDS 経由でのみゲストに渡し、ログや state ファイルには残さない。
- Webhook は HMAC-SHA256 で検証する。`github.repositories` / `github.organizations` で対象を限定できる。
- 現状は jailer を使わず root で Firecracker を起動する。ホストを専用にし、信頼できない同居プロセスを置かないこと。
- ゲストの外向き通信は NAT で全開放される。必要なら `scripts/setup-host.sh` の iptables を絞るか、別途 egress 制御を入れる。

## メトリクス

`/metrics` で Prometheus 形式で公開します。

- `fcghact_vms_active`
- `fcghact_vm_start_seconds`
- `fcghact_jobs_dispatched_total{repository}`
- `fcghact_jobs_finished_total{result}`
- `fcghact_job_failures_total{stage}`
- `fcghact_webhooks_total{action,result}`
- `fcghact_queue_depth`

## トラブルシューティング

- `doctor` で前提条件を確認する。
- VM 実行中のコンソールログは `/var/lib/fcghact/vms/<vm-id>/console.log`、Firecracker のログは同ディレクトリの `firecracker.log` にある。VM 終了時に削除されるため、保存したい場合は `firecracker.keep_logs: true` を設定する (保存先は `<state_dir>/logs/<vm-id>/`)。
- 異常終了後に残った VM は `fcghact cleanup` で回収する (起動時にも自動回収される)。
- `permission denied` が出る場合は root で実行しているか、`/dev/kvm` のパーミッションを確認する。
- `install: cannot stat 'app.pem'` が出る場合、`app.pem` はリポジトリに含まれない。GitHub App の
  **Private keys** からダウンロードした実際の `.pem` のパスを `install` に渡す
  (`sudo install -m 600 ~/Downloads/<app-name>.private-key.pem /etc/fcghact/app.pem`)。
- `/.env` や `/.git/config` への 404 がログに出る場合、インターネットスキャナからのアクセス。
  404 は正常で、INFO レベルでは記録されない。外部公開を `/webhook` だけに絞るとなお良い。
- Webhook が届いているかは `fcghact_webhooks_total` とログの `delivery` で確認する。
- OCI の pull が `UNAUTHORIZED` になる場合は、root で `docker login ghcr.io` するか `rootfs.token_env` を設定する。`fcghact rootfs inspect` で参照と認証を切り分けられる。

## 既知の制約

- JIT ランナーは登録後にラベルが合致する任意の queued ジョブを取得できます。`fcghact` 専用ラベルと job ID 単位の claim で多重起動を抑止していますが、厳密な 1:1 割当が必要な場合は ARC の scale set プロトコル実装が必要です。
- rootfs の reflink は btrfs / XFS (reflink=1) でのみ高速です。それ以外はフルコピーになります。
- 1 ホスト 1 コントローラです (state dir の flock で二重起動を防ぎます)。
- ゲストにはコンテナランタイムを入れていないため、Docker コンテナアクションやサービスコンテナは動きません。必要な場合は rootfs にランタイムを追加してください (ネスト仮想化は不可)。
- コントローラ再起動中に届かなかった Webhook は失われます。再起動後に queued のままのジョブは手動で `fcghact run` するか、Webhook を再配信してください。
- jailer / UFFD スナップショット / CNI は未実装です ([docs/design.md](docs/design.md#10-ロードマップ))。

## 開発

```bash
make test    # go test ./...
make vet     # go vet ./...
make fmt     # gofmt
```

`fcghact run` を使うと Webhook なしで 1 ジョブを実行できます。Firecracker が動かない環境では `go test ./...` によりロジックのみ検証できます。
