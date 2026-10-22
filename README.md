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
sudo install -m 600 app.pem /etc/fcghact/app.pem
echo -n 'webhook-secret' | sudo tee /etc/fcghact/webhook_secret >/dev/null

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
3. Webhook URL を `https://<host>/webhook`、Secret を設定する。
4. 対象リポジトリ (または組織) に App をインストールする。
5. Private key をダウンロードして `github.private_key_path` に置く。

`github.scope` は既定の `repo` を推奨します。`org` にすると 1 つのランナーグループで組織内の複数リポジトリを扱えますが、App の組織権限が必要です。

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
guest/agent          VM 内エージェント (MMDS → runner)
images/rootfs        ゲスト rootfs の Dockerfile
scripts              ホストセットアップ用スクリプト
```

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
- VM のコンソールログは `/var/lib/fcghact/vms/<vm-id>/console.log`、Firecracker のログは同ディレクトリの `firecracker.log` に残る。
- 異常終了後に残った VM は `fcghact cleanup` で回収する (起動時にも自動回収される)。
- `permission denied` が出る場合は root で実行しているか、`/dev/kvm` のパーミッションを確認する。
- Webhook が届いているかは `fcghact_webhooks_total` とログの `delivery` で確認する。

## 既知の制約

- JIT ランナーは登録後にラベルが合致する任意の queued ジョブを取得できます。`fcghact` 専用ラベルと job ID 単位の claim で多重起動を抑止していますが、厳密な 1:1 割当が必要な場合は ARC の scale set プロトコル実装が必要です。
- rootfs の reflink は btrfs / XFS (reflink=1) でのみ高速です。それ以外はフルコピーになります。
- 1 ホスト 1 コントローラです (state dir の flock で二重起動を防ぎます)。
- jailer / UFFD スナップショット / CNI は未実装です ([docs/design.md](docs/design.md#9-ロードマップ))。

## 開発

```bash
make test    # go test ./...
make vet     # go vet ./...
make fmt     # gofmt
```

`fcghact run` を使うと Webhook なしで 1 ジョブを実行できます。Firecracker が動かない環境では `go test ./...` によりロジックのみ検証できます。
