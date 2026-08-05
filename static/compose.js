const form = document.querySelector("[data-mail-compose]");

const attachmentForm = document.querySelector("[data-attachment-upload]");
const attachmentInput = document.querySelector("[data-attachment-input]");

if (form) {
  const status = document.querySelector("[data-autosave-status]");
  const sendForm = document.querySelector("[data-send-draft]");
  let draftID = form.dataset.draftId;
  let dirty = false;
  let timer;
  let saving;

  const endpoint = () => draftID ? `/drafts/${draftID}/autosave` : "/drafts/autosave";
  const setStatus = (text) => {
    if (status) status.textContent = text;
  };
  const save = async () => {
    clearTimeout(timer);
    if (!dirty && draftID) return;
    setStatus("Saving…");
    const response = await fetch(endpoint(), {method: "POST", body: new FormData(form)});
    if (!response.ok) {
      setStatus("Not saved");
      throw new Error(await response.text());
    }
    const result = await response.json();
    dirty = false;
    setStatus("Saved");
    if (!draftID) window.location.replace(result.url);
  };
	if (attachmentForm && attachmentInput) {
		attachmentInput.addEventListener("change", async () => {
			if (!attachmentInput.files?.length) return;
			const attachmentStatus = document.querySelector("[data-attachment-status]");
			const sendButton = document.querySelector('[form="send-draft"]');
			if (attachmentStatus) attachmentStatus.textContent = "Attaching…";
			if (sendButton) sendButton.disabled = true;
			try {
				if (saving) await saving;
				if (dirty) await save();
				attachmentForm.submit();
			} catch {
				if (attachmentStatus) attachmentStatus.textContent = "Save failed — file not attached";
				if (sendButton) sendButton.disabled = false;
			}
		});
	}
  const schedule = () => {
    dirty = true;
    setStatus("Unsaved");
    clearTimeout(timer);
    timer = setTimeout(() => { saving = save().catch(() => {}); }, 900);
  };

  form.addEventListener("input", schedule);
  form.addEventListener("change", schedule);
  if (!draftID) {
    dirty = true;
    saving = save().catch(() => setStatus("Could not create draft"));
  }
  if (sendForm) {
    sendForm.addEventListener("submit", async (event) => {
      if (!dirty && !saving) return;
      event.preventDefault();
      try {
        if (saving) await saving;
        if (dirty) await save();
        sendForm.submit();
      } catch {
        setStatus("Save failed — message not sent");
      }
    });
  }
  window.addEventListener("beforeunload", (event) => {
    if (!dirty) return;
    event.preventDefault();
    event.returnValue = "";
  });
}
