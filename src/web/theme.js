// テーマの切替（index.html と live.html で共通）。<body> の直後で読み込み、描画の前に反映する。
// 設定は Webストレージの ai_agent_room.settings の theme（system: OS に合わせる / light / dark。未設定はダーク）。
// 別の窓で変えたときも storage イベントで反映する。
(function () {
  const KEY = 'ai_agent_room.settings';
  // 改名前の名前（ai_chat.*）で保存した値を一度だけ引き継ぐ。両方の画面で最初に読み込むのでここで行う
  for (const name of ['settings', 'human']) {
    try {
      const old = localStorage.getItem('ai_chat.' + name);
      if (old !== null && localStorage.getItem('ai_agent_room.' + name) === null) localStorage.setItem('ai_agent_room.' + name, old);
      localStorage.removeItem('ai_chat.' + name);
    } catch (e) { console.warn('settings.migrate', e); }
  }
  const osLight = matchMedia('(prefers-color-scheme: light)');
  function themePref() {
    try { return (JSON.parse(localStorage.getItem(KEY)) || {}).theme || 'dark'; } catch { return 'dark'; }
  }
  function applyTheme() {
    const p = themePref();
    document.body.classList.toggle('theme-light', p === 'light' || (p === 'system' && osLight.matches));
  }
  applyTheme();
  osLight.addEventListener('change', applyTheme);
  window.addEventListener('storage', (e) => { if (e.key === KEY) applyTheme(); });
  window.themePref = themePref;
  window.applyTheme = applyTheme;
})();
