// theme.js (TEN-547): light or dark. Loaded blocking in <head> so the first
// paint has the right scheme. The choice is kept in localStorage; with none,
// it follows the computer's setting, live. The only hand-written JS besides
// Datastar: the scheme class here, and the chat's scroll at the end.
// theme-switch turns the fades off for the two frames around a switch
// (styles.css).
const root = document.documentElement, os = matchMedia("(prefers-color-scheme: dark)");
const apply = () => {
  const t = localStorage.getItem("tenant-theme");
  root.classList.add("theme-switch");
  root.classList.toggle("wa-dark", t ? t === "dark" : os.matches);
  requestAnimationFrame(() => requestAnimationFrame(() => root.classList.remove("theme-switch")));
};
apply(); os.addEventListener("change", apply);
addEventListener("click", (e) => {
  if (!e.target.closest("#theme-toggle")) return;
  localStorage.setItem("tenant-theme", root.classList.contains("wa-dark") ? "light" : "dark"); apply();
});

// Chat follow: the chat log and the activity rail stay at the newest line as
// lines arrive, like any chat. Scrolling up stops that until you scroll back
// to the bottom or send a message.
addEventListener("DOMContentLoaded", () => {
  for (const el of document.querySelectorAll("#chat-log, #activity-rail")) {
    let follow = true;
    const down = () => { el.scrollTop = el.scrollHeight; };
    el.jump = () => { follow = true; down(); };
    el.addEventListener("scroll", () => { follow = el.scrollHeight - el.scrollTop - el.clientHeight < 48; });
    new MutationObserver(() => follow && down()).observe(el, { childList: true, subtree: true, characterData: true });
    down();
  }
});
addEventListener("submit", (e) => e.target.matches(".composer") && document.getElementById("chat-log")?.jump?.());
