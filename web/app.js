const listEl = document.getElementById("cameras");
const statusEl = document.getElementById("status");
const refreshBtn = document.getElementById("refresh");
const activePreviews = new Set();

async function fetchCameras(force = false) {
  if (!force && activePreviews.size > 0) {
    return;
  }
  statusEl.textContent = "Refreshing...";
  try {
    const res = await fetch("/api/cameras");
    const data = await res.json();
    renderCameras(data);
    statusEl.textContent = `Found ${data.length}`;
  } catch (err) {
    statusEl.textContent = "Failed to load";
  }
}

function renderCameras(cameras) {
  listEl.innerHTML = "";
  if (!cameras.length) {
    listEl.innerHTML = "<p class=\"muted\">No cameras detected.</p>";
    return;
  }

  cameras.forEach((cam) => {
    const card = document.createElement("div");
    card.className = "camera";

    const info = document.createElement("div");
    info.innerHTML = `
      <div class="camera-title">${cam.name}</div>
      <div class="camera-meta">${cam.node}</div>
      <div class="camera-meta">Stream: ${cam.streamPath}</div>
    `;

    const preview = document.createElement("div");
    preview.className = "preview";
    preview.innerHTML = `
      <img alt="Preview" />
    `;

    function startPreview() {
      const img = preview.querySelector("img");
      img.src = `/api/preview?deviceUid=${encodeURIComponent(cam.deviceUid)}`;
      preview.classList.add("active");
    }

    function stopPreview() {
      const img = preview.querySelector("img");
      img.removeAttribute("src");
      preview.classList.remove("active");
    }

    const toggle = document.createElement("button");
    toggle.textContent = cam.enabled ? "Stop Streaming" : "Start Streaming";
    toggle.addEventListener("click", async () => {
      toggle.disabled = true;
      await fetch("/api/cameras/toggle", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ deviceUid: cam.deviceUid, enabled: !cam.enabled })
      });
      await fetchCameras();
      toggle.disabled = false;
    });

    const previewBtn = document.createElement("button");
    previewBtn.className = "ghost";
    const previewActive = activePreviews.has(cam.deviceUid);
    previewBtn.textContent = previewActive ? "Stop Preview" : "Start Preview";
    previewBtn.addEventListener("click", () => {
      if (preview.classList.contains("active")) {
        stopPreview();
        activePreviews.delete(cam.deviceUid);
        previewBtn.textContent = "Start Preview";
        return;
      }
      startPreview();
      activePreviews.add(cam.deviceUid);
      previewBtn.textContent = "Stop Preview";
    });

    const actions = document.createElement("div");
    actions.className = "toggle";
    actions.append(previewBtn, toggle);

    card.append(info, preview, actions);
    listEl.append(card);

    if (previewActive) {
      startPreview();
    }

  });
}

refreshBtn.addEventListener("click", () => fetchCameras(true));
fetchCameras(true);
setInterval(fetchCameras, 10000);
