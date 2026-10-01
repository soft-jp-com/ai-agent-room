// 画面の多言語化（案 14）。対象は画面の文言だけで、エージェントに渡す指示文・チャットの本文・サーバが返すメッセージは訳さない。
// 日本語の文言をそのままキーにして、英語の訳を引く。訳がない文言は日本語のまま出す。
// 言語は設定（Webストレージ ai_agent_room.settings の lang）に保存し、両方の画面（チャット・別窓）で共有する。
const I18N_EN = {
  // ---- チャット画面（固定の文言） ----
  '設定': 'Settings',
  '間隔・上限・進め方・進行役・周回・作業ディレクトリ': 'Interval, limit, mode, leader, rounds, working directory',
  '停止': 'Stop',
  '新しい会話': 'New chat',
  '履歴と各エージェントのセッションを破棄': 'Discard the history and every agent session',
  '要約して新しい会話': 'Summarize & new chat',
  '進行役（いなければ最初のエージェント）が会話を要約し、要約を引き継いで新しい会話を始める。元のログは残る':
    'The leader (or the first agent) summarizes the chat and starts a new chat with the summary. The original log is kept.',
  'ログ': 'Logs',
  '間隔': 'Interval',
  '秒': 'sec',
  'エージェントが話し終わってから次のエージェントを起動するまでの待ち時間': 'Wait time after an agent finishes before starting the next agent',
  '上限': 'Limit',
  'チャットで、人間の発言1回あたりにエージェントが自動で発言できる最大回数': 'Maximum number of agent messages per human message',
  'セッション切替': 'Session rotation',
  'トークン': 'tokens',
  '人間が送信したとき、直近1回の実行の入力（キャッシュ分を含む）がこの値を超えたエージェントは、議事録を作って CLI セッションを切り替える。0 で切り替えない':
    'When a human sends a message, agents whose last input (including cache) exceeds this value write minutes and switch to a new CLI session. 0 disables it.',
  '応答の上限': 'Response limit',
  '分': 'min',
  '1ターン（CLI の1回の起動）の上限時間。これを過ぎると中止して AGENT_TIMEOUT を出す。エージェントごとに変える場合は下の一覧で指定する':
    'Time limit for one turn (one CLI run). Past it the run is aborted with AGENT_TIMEOUT. Set per-agent values in the list below.',
  'このエージェントだけの応答の上限（分）。空欄なら会話全体の設定に従う': 'Response limit for this agent only (minutes). Empty uses the conversation-wide setting.',
  '↻ 再試行': '↻ Retry',
  '同じ新着を渡して、このエージェントをもう一度起動する': 'Run this agent again with the same new messages',
  '進め方': 'Mode',
  '会話が進んでいないときに「送信」で始める進め方。フリートーク: 全員が新しい発言を見て話したいときに発言し続ける。順番に発言: 決まった順番で指定周回数だけ議論する。チャット: 宛先（なければ全員）が返事をする。返事の中で別のAIが @ID で呼ばれると、呼ばれたAIが続けて発言する（上限まで）':
    'How "Send" starts a conversation when none is running. Free talk: everyone reads new messages and speaks whenever they want. Take turns: discuss in a fixed order for the given rounds. Chat: the addressees (or everyone) reply; an AI called with @ID in a reply speaks next (up to the limit).',
  'フリートーク': 'Free talk',
  '順番に発言': 'Take turns',
  'チャット': 'Chat',
  '進行役': 'Leader',
  '進行役は作業を割り振り、人間の依頼の範囲内で進めるかどうかを判断する。変更は次の発言から反映':
    'The leader assigns work and decides how to proceed within the human\'s request. Changes apply from the next message.',
  '周回': 'Rounds',
  '全員が1回ずつ発言して1周': 'One round = everyone speaks once',
  '作業ディレクトリ:': 'Working directory:',
  'エージェントが作業するディレクトリ。変更すると新しい会話を始める。サーバを再起動しても引き継ぐ':
    'The directory agents work in. Changing it starts a new chat. Kept across server restarts.',
  '変更': 'Change',
  'エージェント': 'Agents',
  '追加するエージェントの種類。同じ CLI をもう1つ参加させる（例: Codex の利用枠が尽きたときに Claude Code をもう1つ入れる）':
    'Type of agent to add. You can add another instance of the same CLI (e.g. another Claude Code when Codex runs out of quota).',
  '追加': 'Add',
  '追加・削除は会話が進んでいないときだけできる。既定の3つは削除できない。': 'Agents can be added or removed only while no conversation is running. The three default agents cannot be removed.',
  '変更はすぐに反映され、ブラウザに保存される（作業ディレクトリは「変更」で反映し、サーバーに保存される）。':
    'Changes apply immediately. The interval, limit, session rotation and leader are saved on the server; mode, rounds, language and theme are saved in this browser. The working directory applies with "Change".',
  '閉じる': 'Close',
  '設定フォルダを開く': 'Open config folder',
  'ルール（rules.md）やエージェントのできること（capabilities.json）を置くフォルダをエクスプローラーで開く。エージェントはこのフォルダを読み書きできない':
    'Open the folder that holds the rules (rules.md) and agent capabilities (capabilities.json). Agents cannot read or write this folder.',
  '宛先:': 'To:',
  '（宛先なし＝全員が同時に回答。AIが @名前 で呼ぶと、呼ばれたAIが続けて発言します）': '(No addressee = everyone answers at once. When an AI calls @name, that AI speaks next.)',
  'メッセージ（Enterで送信 / Shift+Enterで改行）。会話が進んでいないときは、「設定」の「進め方」で始める':
    'Message (Enter to send / Shift+Enter for a new line). When no conversation is running, it starts with the "Mode" in Settings.',
  '送信': 'Send',
  'フリートーク・順番に発言の最中はその会話に加わる。進んでいないときは、「設定」の「進め方」で選んだ方法で始める（入力はお題になる）':
    'Joins the running free talk or turn-taking. Otherwise starts with the "Mode" chosen in Settings (the input becomes the topic).',
  'テーマ': 'Theme',
  '画面の配色。別窓（CLI 出力）にも反映する': 'Color scheme of the screen. Also applies to the CLI output window.',
  'OS に合わせる': 'Follow OS',
  'ライト': 'Light',
  'ダーク': 'Dark',
  '言語': 'Language',
  '画面の表示言語（エージェントへの指示やチャットの本文は変わらない）': 'Display language of this screen (instructions to agents and chat messages are not translated)',

  // ---- チャット画面（スクリプトで出す文言） ----
  'あなた': 'You',
  'サーバーに接続できません': 'Cannot connect to the server',
  '#{0} まで読了': 'read up to #{0}',
  '↩ #{0} への返信': '↩ reply to #{0}',
  '返信': 'Reply',
  'この発言に返信': 'Reply to this message',
  '返信をやめる': 'Cancel reply',
  '作業ディレクトリ: {0}': 'Working directory: {0}',
  'なし': 'None',
  '（未インストール）': ' (not installed)',
  '削除': 'Remove',
  'CLI の出力をストリーミングで見る（別窓）': 'Watch the CLI output live (new window)',
  '出力': 'Output',
  'モデル（次の発言から反映）': 'Model (applies from the next message)',
  '直近の発言のモデル: {0}': 'Model of the last message: {0}',
  'プランの残り利用枠: {0}': 'Plan quota: {0}',
  '作業中': 'Editing',
  '考え中': 'Thinking',
  '一時停止中': 'Paused',
  '様子見': 'Waiting',
  '聞いている': 'Listening',
  '未インストール': 'Not installed',
  '待機': 'Idle',
  '未参加': 'Not joined',
  'ディスカッション中': 'Discussion',
  '第{0}周 / {1}周・次: {2}': 'Round {0} / {1} · next: {2}',
  'お題: {0}': 'Topic: {0}',
  'フリートーク中': 'Free talk',
  'AIの発言 {0} / {1}（人間が発言するとリセット）': 'AI messages {0} / {1} (resets when a human speaks)',
  '既定': 'Default',
  '一時停止': 'Pause',
  '{0}秒': '{0}s',
  '次: {0}（あと{1}秒）… 今のうちに発言すると、次の発言者に渡されます': 'Next: {0} (in {1}s)… Anything you send now goes to the next speaker.',
  'お題を入力してください': 'Please enter a topic',
  '会話を要約してから、履歴と各エージェントのセッションを破棄して新しい会話を始めますか？（元のログは残ります）':
    'Summarize the chat, then discard the history and every agent session and start a new chat? (The original log is kept.)',
  '履歴と各エージェントのセッションを破棄して、新しい会話を始めますか？': 'Discard the history and every agent session and start a new chat?',
  '作業ディレクトリを {0} に変更しますか？今の会話は残し、変更先で前に話していた会話があればそれを復元します（なければ新しい会話を始めます）。':
    'Change the working directory to {0}? The current chat is kept, and the chat you last had in that directory is restored (a new chat starts if there is none).',
  'ログ一覧': 'Logs',
  'チャットに戻る': 'Back to chat',
  '（人間の発言なし）': '(no human messages)',
  '現在の会話 · ': 'Current chat · ',
  '{0}件': '{0} messages',
  'ログはまだありません。': 'No logs yet.',
  '@{0} を削除しますか？（この会話でのセッションは破棄されます）': 'Remove @{0}? (Its session in this chat is discarded.)',

  // ---- 会話の検索・書き出し（案 8.4） ----
  '検索': 'Search',
  '過去ログを横断して発言を探す': 'Search messages across all logs',
  'Markdown で書き出す': 'Export as Markdown',
  '現在の会話': 'Current chat',
  '見つかりませんでした。': 'No matches.',
  '{0}件（{1}件のログを検索）': '{0} matches (searched {1} logs)',
  '{0}件中、新しい{1}件を表示（{2}件のログを検索）': 'Showing the {1} newest of {0} matches (searched {2} logs)',

  // ---- コードブロックの実行（案 7.1） ----
  'コマンドの実行': 'Run commands',
  'チャットのコードブロック（powershell・cmd・bash など）に付ける［実行］ボタンを、進行役以外の発言にも出す。進行役以外のコマンドは、実行の前に確認する。変更は次に表示する発言から反映':
    'Show the Run button on code blocks (powershell, cmd, bash, etc.) in messages from agents other than the leader too. Commands from others ask for confirmation first. Applies to messages shown after the change.',
  'すべての発言に出す': 'On all messages',
  '進行役の発言だけ': 'Leader\'s messages only',
  '実行': 'Run',
  '中止': 'Cancel',
  '作業フォルダ': 'Folder',
  'コマンドを実行するフォルダ。既定は会話の作業ディレクトリ': 'Folder to run the command in. Defaults to the chat\'s working directory.',
  '結果をチャットに出さない': 'Don\'t post the result',
  '結果をチャットに投稿しない（エージェントに渡さない）。全文ログも作業ディレクトリの外に置く':
    'Don\'t post the result to the chat (agents don\'t see it). The full log is also kept outside the working directory.',
  '⚠ 削除や本番の更新を含む可能性があります。': '⚠ This may delete data or change production. ',
  'エージェントの権限の外で、あなたの権限で実行されます。': 'Runs with your permissions, outside the agents\' permissions.',
  '⚠ AI Agent Room の設定や許可ルールなど、保護しているファイルに触れるコマンドです。': '⚠ This command touches protected files such as AI Agent Room settings or permission rules. ',
  '（このエージェントには禁止ルールを渡せないため、実行前に必ず確認します）': ' (This agent cannot be given deny rules, so you are always asked to confirm.)',
  '⚠ 保護しているファイルや削除・本番の更新に触れる可能性があります。': '⚠ This may touch protected files, delete data or change production.',
  '実行中': 'Running',
  '終了（終了コード {0}）': 'Finished (exit code {0})',
  '失敗': 'Failed',
  '中止しました': 'Canceled',
  '時間切れで止めました': 'Stopped (time limit)',
  '中断': 'Interrupted',
  '発言から1時間を過ぎたので実行できません': 'Cannot run: more than an hour has passed since the message',
  '時間制限': 'Time limit',
  '分': 'min',
  '実行を打ち切るまでの時間（最大120分）': 'Time before the command is stopped (up to 120 min)',
  '全文ログを残さない': 'Keep no full log',
  '出力の全文をファイルに残さない。秘密が出るコマンドに使う': 'Do not save the full output to a file. Use this for commands that print secrets.',
  '{0} の発言にあるコマンドを、あなたの権限で実行します。内容を確認しましたか？': 'Run the command from {0}\'s message with your permissions? Have you checked its content?',

  // ---- あなたへ（案 7.2 の第一段階） ----
  'あなたへの依頼の一覧を開く・閉じる': 'Open or close the list of requests for you',
  'あなたへ: 未確認 {0}件 · 済みにしていないもの {1}件（押すと一覧を開く）': 'For you: {0} unchecked · {1} not done (click to open the list)',
  'あなたへの依頼（済みにしていないもの）': 'Requests for you (not done)',
  '進行役にまとめを頼む': 'Ask the leader for a summary',
  '「人間へのお願い（未処理）と完了したことを、1つの発言にまとめてください」と進行役に頼む':
    'Ask the leader to summarize pending requests for you and what has been completed in one message',
  'コマンド': 'Command',
  '確認': 'Check',
  'この発言へ移る': 'Jump to this message',
  '済みにしていない依頼はありません。': 'No pending requests.',
  'あなたへ: 実行してほしいコマンド': 'For you: command to run',
  'あなたへ: 確認・判断のお願い': 'For you: please check / decide',
  'この依頼を片付けたら押す（ブラウザに保存）': 'Press when you have handled this request (saved in this browser)',
  '✓ 済み': '✓ Done',
  '@human 宛て以外を隠す': 'Hide messages not for @human',
  '人間の発言、@human 宛ての発言、実行できるコマンドを含む発言、実行結果、システムの通知だけを表示する':
    'Show only your messages, messages for @human, messages with runnable commands, command results and system notices',
  '非表示 {0}件': '{0} hidden',
  '↺ 未完了に戻す': '↺ Not done',

  // ---- 別窓（CLI 出力） ----
  'CLI 出力': 'CLI output',
  '{0} の CLI 出力': 'CLI output of {0}',
  '{0}（@{1}）の CLI 出力': 'CLI output of {0} (@{1})',
  '接続中…': 'Connecting…',
  '受信中': 'Receiving',
  '切断（自動で再接続します）': 'Disconnected (reconnecting automatically)',
  '最新に追従': 'Follow latest',
  '生の JSON を表示': 'Show raw JSON',
  'CLI が出力した JSON の行をそのまま表示する': 'Show the JSON lines from the CLI as they are',
  '表示を消去': 'Clear',
  'この窓の表示だけを消す（サーバの記録は消えない）': 'Clear this window only (the server keeps its record)',
  'セッション開始': 'Session started',
  'セッション開始（{0}）': 'Session started ({0})',
  'モデル不明': 'unknown model',
  '■ 完了': '■ Done',
  '■ 完了（{0}）': '■ Done ({0})',
  '■ 失敗: {0}': '■ Failed: {0}',
  'エラー: {0}': 'Error: {0}',
  'エラー': 'Error',
  '結果': 'Result',
  '結果（終了コード {0}）': 'Result (exit code {0})',
  '思考': 'Thinking',
  '（サブエージェント）': '(subagent) ',
  '…（長い行のため途中まで）': '… (truncated: line too long)',
  '{0}行': '{0} lines',
  // ---- 権限の段階（案8） ----
  '既定（起動時の設定のまま）': 'Default (as configured at startup)',
  '読み取りのみ': 'Read-only',
  '作業フォルダに書き込み可': 'Write in the working folder',
  'この CLI は画面から権限を変えられません': 'The permissions of this CLI cannot be changed from the screen',
  '権限: 非対応': 'Permissions: not supported',
  '次の発言から反映。守るファイルへの禁止ルールはどの段階でも外さない': 'Applies from the next message. The deny rules for protected files are kept at every level.',
  '権限': 'Permissions',
  '⚠ 起動時の指定（*_ARGS）を上書きしています': '⚠ Overrides the startup options (*_ARGS)',
  'ファイルの書き込みだけを制限します（MCP ツールは制限されません）': 'Only file writes are restricted (MCP tools are not restricted)',
  '@{0} の権限を「{1}」に上げます。エージェントがあなたの PC で行える操作が増えます。よろしいですか？':
    'Raise the permissions of @{0} to "{1}"? The agent will be able to do more on your PC.',
  // ---- 対話モード（案9） ----
  '対話': 'Interactive',
  '対話を終了': 'End interactive',
  'cmd・bat のブロックは、この OS では実行できません': 'cmd/bat blocks cannot be run on this OS',
  'CLI を対話モードで新しい窓に開く（ログイン・認証など）。窓を閉じるまで会話に参加しない':
    'Open the CLI in interactive mode in a new window (for sign-in, auth, etc.). The agent stays out of the chat until the window is closed.',
  '対話中（窓を閉じると戻ります）': 'Interactive (returns when the window is closed)',
  // ---- トークン使用量（案15） ----
  '取得できず {0}回': 'not reported {0}x',
  '使用量: 不明': 'Usage: unknown',
  '直近 {0}': 'last {0}',
  '累計 入力 {0}（キャッシュ {1}）/ 出力 {2}': 'total in {0} (cached {1}) / out {2}',
  '直近1回の実行の入力: {0} トークン': 'Input of the last run: {0} tokens',
  '{0}回の実行で 入力 {1}（うちキャッシュ {2}）/ 出力 {3} トークン': '{0} runs: input {1} (cached {2}) / output {3} tokens',
  '使用量を取得できなかった実行: {0}回（累計に含まない）': 'Runs without usage data: {0} (not included in the totals)',
  '更新: {0}': 'Updated: {0}',
  // ---- 実行待ちのコマンド（案11） ----
  '実行待ちのコマンド {0}件': '{0} commands to run',
  '実行待ちのコマンド（期限内でまだ実行していないもの）': 'Commands waiting to run (not run yet, not expired)',
  'ブロック{0}': 'Block {0}',
  'このブロックへ移る': 'Go to this block',
  '実行待ちのコマンドはありません。': 'No commands waiting to run.',
  // ---- 送信前の秘密情報の警告（案1） ----
  '秘密情報が含まれている可能性があります': 'The message may contain a secret',
  '入力に次の形式の文字列が見つかりました。送信すると全エージェントに渡り、チャットログにも保存されます。':
    'Strings of the following formats were found. If you send it, every agent receives it and it is saved in the chat log.',
  '判定は既知の形式だけです。見つからなくても安全とは限りません。': 'Only known formats are checked. Not finding one does not mean the message is safe.',
  '戻って編集': 'Back to edit',
  'このまま送信': 'Send anyway',
  'API キー（sk-）': 'API key (sk-)',
  'GitHub のトークン': 'GitHub token',
  'AWS のアクセスキー': 'AWS access key',
  'Slack のトークン': 'Slack token',
  'Google の API キー': 'Google API key',
  '秘密鍵': 'Private key',
  // ---- 過去の会話から分岐（案3） ----
  'ここから分岐': 'Branch from here',
  'この発言までを引き継いで新しい会話を始める。元のログは変更しない': 'Start a new chat that carries over the messages up to this one. The original log is not changed.',
  '{0} の #{1} までを引き継いで、新しい会話を始めますか？今の会話は終わります（ログは残ります）。':
    'Start a new chat carrying over {0} up to #{1}? The current chat ends (its log is kept).',
  // ---- CLI の診断（案6） ----
  '診断': 'Diagnose',
  '各エージェントの CLI を --version で起動し、見つかるか・起動できるかを確かめる（押したときだけ実行する）':
    "Run each agent's CLI with --version to check that it is found and starts (runs only when pressed)",
  '診断しています…': 'Diagnosing…',
  '✓ 取得できた': '✓ OK',
  '✗ 見つからない': '✗ Not found',
  '✗ 起動に失敗した': '✗ Failed to start',
  '✗ 時間切れ': '✗ Timed out',
  '— 対象外': '— Not applicable',
  // ---- 大量のログの検索（案4） ----
  '{0}件以上、新しい{1}件を表示（{2}件のログを検索して打ち切り）': '{0}+ matches, showing the newest {1} (stopped after searching {2} logs)',
  // ---- 背面にあるときの通知（案7） ----
  '背面にあるとき通知する': 'Notify when in background',
  'このタブが見えていないとき、エージェントの発言・エラー・あなたへの依頼の増加をブラウザの通知で知らせる。本文や作業フォルダは通知に含めない。オンにしたときだけブラウザの許可を求める':
    'When this tab is not visible, show a browser notification for agent messages, errors and new requests to you. Message text and the working directory are not included. Browser permission is requested only when you turn this on.',
  'あなたへの依頼が {0}件あります': '{0} requests to you are open',
  '{0} が発言しました': '{0} posted a message',
  'エージェントのエラー（{0}）': 'Agent error ({0})',
  'このブラウザは通知に対応していません': 'This browser does not support notifications',
  '通知が許可されていません。ブラウザのサイトの設定で許可してください': 'Notifications are not allowed. Allow them in the browser site settings.',
  // ---- 返答本文の途中表示（案5） ----
  '書いています…': 'Writing…',
};

const I18N_LANGS = [['ja', '日本語'], ['en', 'English']];

function i18nLoadLang() {
  try { return (JSON.parse(localStorage.getItem('ai_agent_room.settings')) || {}).lang === 'en' ? 'en' : 'ja'; } catch { return 'ja'; }
}
let lang = i18nLoadLang();

// t は文言を今の言語にする。{0}, {1} … は args で置き換える
function t(s, ...args) {
  const r = (lang === 'en' && I18N_EN[s]) || s;
  return args.length ? r.replace(/\{(\d+)\}/g, (_, i) => String(args[i] ?? '')) : r;
}

// translateDom は root の中の固定の文言（テキスト、title、placeholder）を今の言語にする。
// 元の日本語を覚えておき、言語を切り替えるたびにそこから訳し直す。
const i18nOrig = new WeakMap(); // ノード → 元の日本語 / 要素 → { title, placeholder }
function translateDom(root) {
  document.documentElement.lang = lang;
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    if (n.parentElement && /^(SCRIPT|STYLE|TEXTAREA)$/.test(n.parentElement.tagName)) continue;
    if (!i18nOrig.has(n)) {
      if (!I18N_EN[n.nodeValue.trim()]) continue; // 訳のない文言（名前・数値など）は触らない
      i18nOrig.set(n, n.nodeValue);
    }
    const orig = i18nOrig.get(n);
    const key = orig.trim();
    n.nodeValue = orig.replace(key, t(key));
  }
  for (const el of root.querySelectorAll('[title], [placeholder]')) {
    if (!i18nOrig.has(el)) i18nOrig.set(el, { title: el.getAttribute('title'), placeholder: el.getAttribute('placeholder') });
    const o = i18nOrig.get(el);
    if (o.title && I18N_EN[o.title]) el.title = t(o.title);
    if (o.placeholder && I18N_EN[o.placeholder]) el.placeholder = t(o.placeholder);
  }
}

// setLang は言語を切り替えて保存する
function setLang(l) {
  lang = l === 'en' ? 'en' : 'ja';
  let saved = {};
  try { saved = JSON.parse(localStorage.getItem('ai_agent_room.settings')) || {}; } catch { /* 壊れていれば作り直す */ }
  saved.lang = lang;
  try { localStorage.setItem('ai_agent_room.settings', JSON.stringify(saved)); } catch (e) { console.warn('settings.save', e); }
}
