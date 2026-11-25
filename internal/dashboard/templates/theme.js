// theme.js (TEN-547): light or dark. Loaded blocking in <head> so the first
// paint has the right scheme. The choice is kept in localStorage; with none,
// it follows the computer's setting, live. The only hand-written JS besides
// Datastar, and it touches nothing but the scheme class. theme-switch turns
// the fades off for the two frames around a switch (styles.css).
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
