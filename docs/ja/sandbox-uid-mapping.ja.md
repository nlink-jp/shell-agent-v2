# サンドボックス UID マッピング (v0.4.3)

## 1. 症状

企業管理 macOS アカウントを使用しているユーザーから、
サンドボックスがコンテナを起動しようとした際に下記のエラー
が発生するとの報告があった:

```
Error: ensure container: sandbox: start container:
podman: Error: crun: setresuid to `202594884`: Invalid argument:
OCI runtime error
```

サンドボックスイメージのビルド自体は成功しており、失敗は
コンテナの起動段階に限定されていた。

## 2. 根本原因

`internal/sandbox/cli.go` の `buildRunArgs` がホスト UID を
そのまま `--user` に渡していた:

```go
"--user", strconv.Itoa(os.Getuid()),
```

通常の Linux / macOS マシンであれば、ユーザーの UID
(たとえば 501) は rootless Podman の subuid レンジ (一般的
には `2^31` 未満のどこかから始まる 65,536 個分) 内に収まる
ため動作する。しかし Active Directory / LDAP からマッピング
された企業管理 macOS アカウントでは `os.Getuid()` が
**非常に大きな値** (たとえば 202,594,884) を返すことがあり、
この値は rootless Podman ユーザーネームスペースのマッピング
可能レンジから完全に外れる。結果として `crun` の
`setresuid()` syscall が `EINVAL` で失敗する — 要求された
UID が `/proc/self/uid_map` に存在しないためだ。

報告は `podman` に対するものであった。Docker は別の rootless
モデルを採用しており (macOS の Docker Desktop は専用 VM 内で
実質 rootful + ファイル共有層経由)、同じ症状は再現しない。

## 3. 修正

`podman` エンジンに対しては、`--user` への直接 UID 引渡しを
やめ、コンテナ内で **小さな UID** にマッピングし直すユーザー
ネームスペース指定に切り替える:

```go
args = append(args,
    "--userns", "keep-id:uid=1000,gid=1000",
    "--user", "1000:1000",
)
```

意味:

- `--userns=keep-id:uid=1000,gid=1000` — デフォルトの
  `keep-id` (ホスト UID をコンテナ内で同一 UID にマップ;
  これも巨大 UID では同様の理由で失敗する) と異なり、
  **ホスト UID をコンテナ内 UID 1000 にリマップ**する。
  逆方向のマッピングも対称的にセットアップされるので、
  bind マウントされた `/work` 内に書き出されたファイルは
  ホスト側でホスト UID 所有として見える — 元の
  `--user $(id -u)` が保持しようとしていた性質はそのまま。
- `--user 1000:1000` — コンテナプロセスを実際に UID/GID 1000
  として動かす。これが無いと image の `USER` ディレクティブ
  が決定し、`python:3.12-slim` では root になる。これは
  v0.2.0 サンドボックス設計 (history/sandbox-execution.ja.md
  §9-6) の defense-in-depth 方針を弱めるため避ける。

docker エンジン経路は変更なし: `--user $(id -u)` はrootful
Docker Desktop および rootless Linux Docker 双方で従来通り
動作する。これらの環境では rootless マッピング (存在する
場合) が別の仕組みで配線されているため。

エンジン選択は実行時に既存の `usePodmanUserns(binary)`
ヘルパーで行う — `useSELinuxRelabel` と並列のパターンで、
基底名が `podman` かどうかの 1 行マッチ。

## 4. 互換性

- **Podman バージョン**: `--userns=keep-id:uid=N,gid=N` は
  **Podman 4.3** (2022 年 11 月) で追加された。現在メンテナンス
  されている Podman リリースはすべて対応している。Podman 4.2
  以下では実行時にフラグが拒否される; agent は `podman` の
  エラーメッセージを既存の `sandbox: start container:` ラップ
  経由でそのまま報告する。
- **`/work` のファイル所有権**: 観測上の挙動は変わらない。
  コンテナ内で UID 1000 として作成されたファイルはホスト側
  ではホスト UID として見える — `keep-id` が逆マッピングを
  インストールするため。
- **セキュリティ姿勢**: 変更なし。コンテナプロセスは依然と
  して非 root ユーザー (ネームスペース内の UID 1000) として
  実行される — v0.2.0 サンドボックスセキュリティノートは
  引き続き有効。

## 5. テスト

`internal/sandbox/cli_test.go` に追加:

- `TestBuildRunArgs_PodmanRemapsHostUID` — podman 経路が
  `--userns=keep-id:uid=1000,gid=1000` と `--user 1000:1000`
  の両方を出力し、**ホスト UID を絶対に渡さない**ことを
  アサート。`strconv.Itoa(os.Getuid())` 形式への偶然のリグレ
  ッションをガードする。
- `TestBuildRunArgs_NonPodmanPassesHostUID` — docker 経路が
  歴史的な `--user $(id -u)` 動作を維持し、`--userns` を
  決して出力しないことをアサート。両経路は排他的。
- `TestUsePodmanUserns` — エンジン検出ヘルパーの基底名
  マッチテーブル (大文字小文字無視、フルパス対応)。

新しい `buildRunArgs` のアリティに合わせて既存テストを更新。

## 6. スコープ外

- 1000 以外の UID 選択 (ユースケースが無く、誰も求めていない
  config ノブを増やすだけ)。
- ホスト UID を検出して戦略を動的に切替 (keep-id 経路は
  小さい UID も含め**全ての**ホスト UID で動作するため、
  分岐する価値がない)。
- Podman rootless モデル全般のドキュメント化 (これはこの
  修正ノートではなく history ノートの仕事)。
