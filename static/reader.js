window.addEventListener("message", (event) => {
  if (event.source === null || event.data?.type !== "mail-html-height") return;

  for (const frame of document.querySelectorAll("iframe[data-mail-html]")) {
    if (frame.contentWindow !== event.source) continue;
    const height = Number(event.data.height);
    if (Number.isFinite(height) && height > 0) {
      frame.style.height = `${Math.max(512, Math.ceil(height))}px`;
    }
    break;
  }
});
