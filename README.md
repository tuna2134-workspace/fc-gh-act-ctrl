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
#    runner のバージョンは既定で 2.338.1。必要なら RUNNER_VERSION で上書きする
#    Docker でビルドするジョブを動かす場合は rootfs を大きめにする (例: 8G)
sudo ROOTFS_SIZE=8G ./scripts/build-rootfs.sh

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

## rootfs のサイズ変更

**実行中の VM の rootfs は拡張できません**。Firecracker の virtio-blk 容量は起動時にファイルサイズから決まり、実行中に変更する API はありません。ベースイメージを大きくして、**新しい VM** に反映します。

### エージェントが自動で拡張する場合

新しい rootfs（`resize2fs` 実行機能入り）を使っている場合、ベースファイルを大きくするだけで、次回以降の VM が起動時に ext4 を拡張します。

```bash
# 既存のジョブには影響しません。新しい VM から 16G になります。
sudo truncate -s 16G /var/lib/fcghact/rootfs.ext4
```

ゲストのコンソールに `root filesystem resized: ...` が出ます。実行中の VM を拡張したい場合は、そのジョブを終了させて新しい VM を起動してください。

### エージェントを更新せずに今すぐ拡張する場合

サービスを止めてベースイメージのファイルシステム自体を拡張します。

```bash
sudo systemctl stop fcghact
sudo truncate -s 16G /var/lib/fcghact/rootfs.ext4
sudo e2fsck -f /var/lib/fcghact/rootfs.ext4
sudo resize2fs /var/lib/fcghact/rootfs.ext4
sudo systemctl start fcghact
```

実行中のジョブは停止します。**縮小はしないでください**。作業前にバックアップを取ってください。

### 注意

- OCI の rootfs を使っている場合、キャッシュはレイヤのダイジェスト単位で管理されます。サイズを変える場合は大きい rootfs をビルドして push し、`rootfs.oci_reference` を更新してください。
- Firecracker には PCI デバイスのホットプラグ機能（developer preview）があり、**新しい** virtio-block を実行中 VM に追加できます。ただし `--enable-pci` と `pcie_hotplug_ports`、ゲストカーネルの PCIe hotplug 対応が必要で、rootfs 自体の拡張はできません。本コントローラでは使用していません。
- Docker のビルドデータは rootfs 上に置かれるため、8G 以上を推奨します。

## 起動時間の改善

起動時間の内訳はログとメトリクスで確認できます。

- `microvm started`: `prepare_ms`（ネットワーク確保 + rootfs 準備）、`boot_ms`（Firecracker 起動）、`pooled`（事前クローンを使ったか）
- `job picked up by runner`: `queue_to_runner`（Webhook 受付からランナーがジョブを取得するまで）、`vm_to_runner`（VM 起動から取得まで）
- メトリクス: `fcghact_vm_prepare_seconds`, `fcghact_vm_boot_seconds`, `fcghact_runner_ready_seconds`

### 1. rootfs の事前クローン pool

```yaml
firecracker:
  preclone_pool: 2   # 既定 2。0 で無効
```

`<state_dir>/pool` に rootfs のクローンを保持し、VM 起動時は rename するだけにします。各クローンはベース rootfs の使用量分のディスクを使います。`reflink` 非対応の FS でもコピーをクリティカルパスから外せます。

### 2. reflink の活用

`state_dir` を XFS (reflink=1) や btrfs に置くと `FICLONE` が効き、クローンが即時になります。

### 3. ゲスト起動のチューニング

既定の `boot_args` に `quiet loglevel=3` を追加し、シリアルコンソールへの起動ログを減らしています。あわせて rootfs イメージで `e2scrub_reap` や apt のタイマーなど不要な unit を mask しています。

既存の設定ファイルを使っている場合は `boot_args` を更新してください。

```yaml
firecracker:
  boot_args: "console=ttyS0 quiet loglevel=3 reboot=k panic=1 pci=off net.ifnames=0 root=/dev/vda rw rootfstype=ext4"
```

mask は rootfs イメージ側の変更なので、rootfs の再ビルド（OCI の場合は再 push）が必要です。ランナーの `Listening for Jobs` などの出力は console に残るため、アイドル判定には影響しません。

### 4. それでも足りない場合

warm pool（起動済み VM を MMDS で待機）や Firecracker snapshot + UFFD はロードマップです。現状は 1 ジョブごとに VM をコールドスタートします。

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

`runner.labels` は JIT config に **そのまま** 渡されます。GitHub は `self-hosted` や OS/arch を自動追加しないため、ジョブが要求するラベルをすべて含めてください（既定は `self-hosted,linux,x64,fcghact`）。

### プラン（VM サイズ）を固定する

`plans` に名前付きの VM サイズを定義すると、ジョブは `spec-<name>` ラベルで選択できます。任意のサイズ指定はできず、構成ファイルにあるプランだけが使われます。

```yaml
plans:
  mini-1:
    vcpu_count: 2
    mem_size_mib: 2048
    disk_size: 8G
  mini-2:
    vcpu_count: 4
    mem_size_mib: 8192
    disk_size: 16G
```

```yaml
jobs:
  build:
    runs-on: [self-hosted, fcghact, spec-mini-1]
```

- `spec-<name>` ラベルはランナーに自動で追加されます。
- `vcpu_count` / `mem_size_mib` が 0 または未指定なら `firecracker.*` を継承します。
- `disk_size` が未指定・0 ならベース rootfs のサイズを使います。ベースより小さい値は無視されます（縮小はしません）。
- `plans.default` を定義すると、`spec-*` を指定しないジョブの既定プランになります。未定義なら従来どおり `firecracker.*` の設定です。
- 不明なプラン名や複数の `spec-*` ラベルが付いたジョブは無視され、queued のままになります。プラン定義の変更はコントローラ再起動で反映されます。
- ディスク拡張は VM 起動時にクローンを `truncate` し、ゲストの `resize2fs` でファイルシステムを広げます。
- 手動実行では `--plan` が使えます。

```bash
bin/fcghact run --repo owner/repo --job-id 123 --plan mini-1
```

### リソースラベル

`runner.resource_labels`（既定 `true`）を有効にすると、選択されたプラン（または既定設定）のスペックが JIT ラベルとして自動追加されます。

| ラベル | 例 | 意味 |
|---|---|---|
| `cpu-<n>` | `cpu-4` | vCPU 数 |
| `memory-<n>g` / `memory-<n>m` | `memory-8g` | メモリ (GiB。1024 の倍数でない場合は MiB) |
| `disk-<n>g` / `disk-<n>m` | `disk-16g` | ベース rootfs のファイルサイズ (GiB。倍数でない場合は MiB) |

ジョブ側でサイズを指定できます。

```yaml
runs-on: [self-hosted, fcghact, cpu-4, memory-8g, disk-16g]
```

- `disk-*` は `firecracker.rootfs_image` の実サイズです。ベースを `truncate` で拡張すると、次のジョブから新しい `disk-*` ラベルになります（コントローラ再起動不要）。
- 指定したサイズのランナーがないジョブは queued のままです。ベースを拡張した後にキュー済みだったジョブは、ワークフローを再実行してください。
- 無効化する場合は `runner.resource_labels: false`。
- 複数スペックのコントローラを並べる場合は、`runner.name_prefix` を分けてください。

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
| `runner.labels` | `self-hosted,linux,x64,fcghact` | ランナーが広告する基本ラベル |
| `runner.resource_labels` | `true` | `cpu-*` / `memory-*` / `disk-*` を自動追加 |
| `plans.<name>.vcpu_count` / `mem_size_mib` / `disk_size` | グローバル設定 | `spec-<name>` で選ぶ固定プラン |
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

## Webhook が届かないとき

`app.pem` の配置だけではイベントは発生しません。次の順で確認してください。

1. **GitHub App → Advanced → Recent Deliveries** を開く。
   - 何もない: Webhook URL が未設定、App がリポジトリに未インストール、または GitHub から到達できていない。
   - 4xx/5xx がある: ステータスとレスポンス本文を確認する（401 は Secret 不一致、404 はパス違いなど）。
2. App の **Webhook URL** が `https://<host>/webhook` になっているか、**Active** が有効かを確認する。
3. **Subscribe to events → Workflow jobs** が有効か確認する。
4. App が対象リポジトリにインストールされているか確認する（Repository settings → GitHub Apps）。
5. 外部から到達できるか確認する:

   ```bash
   # 署名なしの POST は 401 が正常。401 ならサーバーまで届いている。
   curl -i -X POST https://<host>/webhook -H 'X-GitHub-Event: ping' -d '{}'

   # GET は 405 (Method Not Allowed) が返れば到達している
   curl -i https://<host>/webhook
   ```

   - 404: リバースプロキシのパス設定が違う
   - timeout / connection refused: ファイアウォール、TLS、`server.listen`、プロキシを確認
6. コントローラのログに `webhook received` が出るか確認する。

   ```
   # 手動起動の場合
   sudo bin/fcghact serve -c /etc/fcghact/fcghact.yaml

   # systemd の場合
   sudo journalctl -u fcghact -f
   ```

   `event=ping` が届けば経路は正常です。`event=workflow_job` が来ない場合は、
   ワークフローが実際に実行されているか、`runs-on` が self-hosted 向けかを確認する。
7. `workflow_job` は **self-hosted ランナーを参照するジョブ**が対象です。
   例: `runs-on: [self-hosted, fcghact]`。`ubuntu-latest` では発生しません。
8. `server.listen` を `127.0.0.1:8080` にしたままプロキシを設定していない、という状態がないか確認する。
9. Webhook Secret は GitHub 側の値と `/etc/fcghact/webhook_secret` の内容を完全一致させる
   (末尾の改行は読み込み時に除去されます)。
10. Cloudflare などの WAF が POST を遮断していないか、GitHub の IP 制限をしていないか確認する。

## 既知の制約

- JIT ランナーは登録後にラベルが合致する任意の queued ジョブを取得できます。`fcghact` 専用ラベルと job ID 単位の claim で多重起動を抑止していますが、厳密な 1:1 割当が必要な場合は ARC の scale set プロトコル実装が必要です。
- rootfs の reflink は btrfs / XFS (reflink=1) でのみ高速です。それ以外はフルコピーになります。
- 1 ホスト 1 コントローラです (state dir の flock で二重起動を防ぎます)。
- 実行中 VM の rootfs は拡張できません。ベースイメージを拡張し、新しい VM で反映してください (「rootfs のサイズ変更」参照)。
- ゲストには `build-essential`（gcc / g++ / make）を同梱しています。ネイティブ拡張のビルドなどに使えます。
- ゲストには Docker を同梱していません。Docker コンテナアクションや `docker build` を使う場合は、`docker/setup-docker-action` などでジョブ中に Docker をセットアップしてください。
  - そのために `iptables` を同梱し、カーネルが `nf_tables` 非対応のため `iptables-legacy` を選択しています。
  - Docker のデータは rootfs 上に置かれるため、`ROOTFS_SIZE` を大きめにしてください (例: 8G)。ビルドには `firecracker.vcpu_count: 4`, `firecracker.mem_size_mib: 4096` 程度を推奨します。
  - `reflink` 非対応のファイルシステムでも、`SEEK_DATA`/`SEEK_HOLE` で割り当て済み領域だけをコピーします (sparse 対応)。XFS (reflink=1) や btrfs ならさらに高速です。
- Docker のネスト仮想化 (VM 内でさらに KVM を使う) はできません。
- GitHub は古い Actions runner を拒否します (`Runner version ... is deprecated and cannot receive messages`)。rootfs の `RUNNER_VERSION` (既定 2.338.1) は定期的に更新してください。最新版は [actions/runner releases](https://github.com/actions/runner/releases) で確認できます。
- コントローラ再起動中に届かなかった Webhook は失われます。再起動後に queued のままのジョブは手動で `fcghact run` するか、Webhook を再配信してください。
- jailer / UFFD スナップショット / CNI は未実装です ([docs/design.md](docs/design.md#10-ロードマップ))。

## 開発

```bash
make test    # go test ./...
make vet     # go vet ./...
make fmt     # gofmt
```

`fcghact run` を使うと Webhook なしで 1 ジョブを実行できます。Firecracker が動かない環境では `go test ./...` によりロジックのみ検証できます。
