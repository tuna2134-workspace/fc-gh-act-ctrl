# fc-gh-act-ctrl 設計書

GitHub Actions の self-hosted runner を Firecracker microVM 上で 1 ジョブ使い捨てで実行するコントローラ。

## 1. 目的

- GitHub Actions のジョブごとに独立した microVM を起動し、ジョブ終了と同時に破棄する。
- ホスト側のコントローラは Webhook を受けて JIT runner を発行し、Firecracker を起動するだけに保つ。
- ランナーの登録・解除を GitHub Actions の JIT config (`generate-jitconfig`) に任せ、登録トークンの
  取り回しやランナー削除 API の呼び出しを不要にする。

### 非目標 (v0.1)

- Actions Runner Controller (ARC) の scale set gRPC プロトコル互換。
- Kubernetes 上での動作 (CRD / operator)。
- マルチホストのスケジューリング。1 プロセス = 1 ホスト。
- ゲストイメージの OCI 配布。rootfs はホスト上のファイルを COW コピーする。

## 2. 全体構成

```
                    ┌──────────────────────────────┐
 GitHub App webhook │  fc-gh-act-ctrl (host, root) │
 ──────────────────▶│                              │
  workflow_job      │  ┌────────────┐   ┌────────┐ │
                    │  │ webhook    │──▶│scheduler│ │
                    │  │ (chi)      │   └───┬────┘ │
                    │  └────────────┘       │      │
                    │        │              ▼      │
                    │        │        ┌──────────┐ │
                    │        │        │ vm mgr   │ │
                    │        │        │ (fc SDK) │ │
                    │        │        └────┬─────┘ │
                    │        ▼             │       │
                    │  ┌────────────┐      │       │
                    │  │ GitHub API │      │       │
                    │  │ (JIT, job) │      │       │
                    │  └────────────┘      │       │
                    └──────────────────────┼───────┘
                                           │ TAP / bridge / NAT
                                 ┌─────────▼──────────┐
                                 │ Firecracker microVM│
                                 │  kernel + rootfs   │
                                 │  guest agent ──▶ actions/runner
                                 │     ▲              │
                                 │     └─ MMDS (169.254.169.254)
                                 └────────────────────┘
```

### コンポーネント

| パッケージ | 役割 |
|---|---|
| `internal/config` | YAML + 環境変数 + デフォルト値 + 検証 |
| `internal/githubapp` | GitHub App 認証、JIT config 発行、job 状態取得 |
| `internal/webhook` | HMAC 検証、`workflow_job` のパース |
| `internal/controller` | ラベル照合、重複排除、スケジューリング、監視、停止 |
| `internal/vm` | Firecracker 起動・停止、rootfs の COW コピー、MMDS、孤児回収 |
| `internal/network` | bridge / TAP 作成、IPAM |
| `guest/agent` | ゲスト内で MMDS を読み JIT config を渡してランナー起動 |
| `cmd/fcghact` | `serve` / `run` / `cleanup` / `doctor` / `version` |

## 3. ジョブライフサイクル

1. GitHub が `workflow_job` (action=`queued`) を Webhook で送る。
2. `webhook` が HMAC を検証し、`controller.HandleWorkflowJob` に渡す。
3. コントローラは以下を満たす場合のみ受理する。
   - `require_labels` をすべて含む (既定: `fcghact`)
   - job のラベル集合が runner のラベル集合の部分集合
   - `github.repositories` / `github.organizations` の許可リストに一致
   - 同一 job ID をまだ claim していない
4. scheduler が空きスロット (全体 / リポジトリ単位) を確認し、`fcghact-j<jobID>-<rand>` という
   一意なランナー名で JIT config を発行する。
5. VM バックエンドが以下を行う。
   - ベース rootfs を reflink (`FICLONE`) で COW コピー (非対応 FS では通常コピー)
   - 空き IP を払い出し、TAP を作成して bridge に接続
   - `ip=` カーネル引数を SDK 経由で設定 (DHCP 不要)
   - Firecracker を起動し、MMDS v2 に JIT config・リポジトリ・DNS を注入
6. ゲストの `fcghact-guest` が MMDS をポーリングしてメタデータを取得し、
   `/etc/resolv.conf` を書き、`run.sh --jitconfig` を実行する。
   MMDS のデータストアは `{"latest":{"meta-data":{...}}}` の形で格納し、
   ゲストは `Accept: application/json` を付けて `/latest/meta-data/` を取得する。
7. ランナーはジョブを 1 つ実行して終了する。ゲストエージェントは `poweroff` する。
8. コントローラは VMM の終了を検知して TAP 削除・IP 返却・ディスク削除を行う。
   同時に `workflow_job` (started/completed) と 30 秒間隔の job 状態ポーリングで
   タイムアウトと早期停止を制御する。

### ライフサイクルの異常系

| 事象 | 挙動 |
|---|---|
| JIT config 発行失敗 | backoff でリトライ (最大 45 秒)。失敗時は claim を解放 |
| VM 起動失敗 | TAP/IP/ディスクを巻き戻し、claim を解放 |
| ランナーがジョブを取らない (idle) | `registration_timeout` 超過で停止 |
| ジョブが `job_timeout` を超過 | VM を強制停止 |
| Webhook が届かない | 監視ループの job 状態ポーリングとコンソールのアイドル判定で停止 |
| ランナーがジョブを取得できない | ジョブが queued のままなら最大 2 回まで内部で再キュー |
| コントローラ再起動 | 起動時に state dir を走査し、残存 VMM/TAP を回収 (reap) |
| 停止要求 | 全 VM に SIGTERM → 10 秒で SIGKILL、リソース解放 |

## 4. 採用ライブラリ

| 領域 | ライブラリ | 採用理由 |
|---|---|---|
| microVM | `github.com/firecracker-microvm/firecracker-go-sdk` | Firecracker 公式 Go SDK。API ソケット、MMDS、TAP、スナップショット、jailer を内包。プロセス起動・終了・エラーハンドリングを自作しない |
| GitHub REST | `github.com/google/go-github/v88` | JIT config、workflow job、installation 解決を型付きで扱える。Webhook の HMAC 検証とイベントパースも同梱 |
| GitHub App 認証 | `github.com/bradleyfalzon/ghinstallation/v2` | App JWT → installation token の取得・更新をラウンドトリッパとして提供 |
| CLI | `github.com/spf13/cobra` | サブコマンド、フラグ、シェル補完 |
| 設定 | `gopkg.in/yaml.v3` + `github.com/caarlos0/env/v11` | YAML を既定値に重ね、環境変数で上書き。Secret はファイル指定も可 |
| HTTP | `github.com/go-chi/chi/v5` | 軽量ルータ。middleware (recover, request id, timeout) を再利用 |
| メトリクス | `github.com/prometheus/client_golang` | VM 数・起動時間・ジョブ結果の可観測性 |
| ネットワーク | `github.com/vishvananda/netlink` | bridge / TAP の作成・削除を netlink で行う |
| OCI 配布 | `github.com/google/go-containerregistry` | rootfs の OCI push / pull、認証、ダイジェスト検証。crane/docker CLI に依存しない |
| リトライ | `github.com/cenkalti/backoff/v4` | GitHub API の 429/5xx に対する指数バックオフ |
| 並行制御 | `golang.org/x/sync` | errgroup による監視ループの並列化 |
| テスト | `github.com/stretchr/testify` | アサーションとモック |
| ID | `github.com/google/uuid` | VM ID / state dir 名 |

意図的に採用しなかったもの:

- **DHCP サーバ (dnsmasq 等)**: ゲスト IP は `ip=` カーネル引数で固定するため不要。
- **CNI プラグイン**: 単一 bridge + NAT で足りる。CNI は将来のマルチテナント向け。
- **独自 Firecracker API クライアント**: SDK で足りる。
- **ARC scale set クライアント**: GitHub 内部 gRPC への依存が大きく、v0.1 では Webhook + JIT で代替。

## 5. ネットワーク設計

- ホストに bridge `fcghact0` (既定 `172.31.0.1/16`) を作成する。
- VM ごとに TAP (`fctap-xxxx`) を作成し bridge に接続する。
- ゲスト IP は IPAM が `172.31.0.2` 以降を払い出す。
- 外向きはホストの `iptables MASQUERADE` (セットアップスクリプトで設定)。
- ゲストの DNS は MMDS メタデータで渡し、エージェントが `/etc/resolv.conf` に書く。
- MMDS は eth0 のみ許可 (`AllowMMDS: true`, MMDS v2)。

## 6. セキュリティ

- microVM がジョブの境界。ジョブは VM 内で root として実行されるが、ホストからは KVM で分離される。
- ゲストの rootfs はジョブごとに COW コピーされ、ジョブ終了時に削除される。
- JIT config は MMDS 経由でのみ渡し、ホストのログには出さない。
- Webhook は HMAC-SHA256 で検証。`X-GitHub-Delivery` をログに残す。
- 許可リスト (`repositories` / `organizations`) で対象を限定できる。
- 将来: jailer による chroot/cgroup、専用ユーザでの Firecracker 実行、seccomp。

## 7. 既知の制約

- **Webhook レース**: JIT ランナーは登録後、ラベルが合致する任意の queued ジョブを取得し得る。
  そのため「特定の job ID のために起動した VM が別の job を実行する」可能性がある。
  対策として `fcghact` 専用ラベルを必須にし、job ID 単位の claim で多重起動を抑止する。
  監視ループは job の runner ID が自分の VM と一致する場合だけ早期停止し、それ以外は
  ゲストのコンソールログ (`Listening for Jobs` / `Running job:`) でアイドルを判定する。
  厳密な 1:1 割当が必要なら ARC の scale set プロトコル実装が必要 (ロードマップ)。
- **同時実行数**: ホストのメモリと vCPU に依存する。`max_concurrent_jobs` で上限を設ける。
- **rootfs コピー**: reflink 非対応 FS ではフルコピーになり起動が遅くなる。
- **Webhook の再送**: GitHub は失敗した配信を自動再送しない。キュー溢れ時は 503 を返し、
  手動再配信または `fcghact run` で復旧する。

## 8. rootfs の OCI 配布

ゲスト rootfs は単一レイヤの OCI イメージとして配布する。

- レイヤ: tar+gzip に `rootfs.ext4` を 1 エントリだけ格納 (`application/vnd.oci.image.layer.v1.tar+gzip`)。
- push: ext4 を tar 化して `tarball.LayerFromFile` に渡し、`empty.Image` に append して
  `remote.Write` する。annotations に source / version / created を付ける。
- pull: `remote.Image` でマニフェストを解決し、`<cache_dir>/sha256-<layer-digest>/rootfs.ext4`
  に展開する。展開前にレイヤの圧縮ダイジェストを SHA-256 で検証する。
- 同時 pull は `<layer-digest>.lock` の flock で排他し、一時ファイル + rename で atomic に置く。
- 認証は Docker keychain を既定とし、`rootfs.token` / `rootfs.token_env` で Bearer トークンを
  指定できる (root で動くデーモンは `/root/.docker/config.json` を参照)。
- `rootfs.prune` で使用中以外のキャッシュを削除する。
- CI (`.github/workflows/rootfs.yml`) が rootfs をビルドして GHCR に push する。

## 9. 設定例

`configs/fcghact.example.yaml` を参照。

## 10. ロードマップ

1. warm pool: 起動済み VM を MMDS ポーリングで待機させ、割当時の起動遅延をなくす。
2. Firecracker snapshot + UFFD によるミリ秒起動。
3. jailer 統合と専用ユーザ実行。
4. CNI によるネットワーク分離。
5. ARC scale set プロトコル対応。
6. OCI イメージのイメージ署名 (cosign) と digest pin の強制。
