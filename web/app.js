const listEl = document.getElementById("cameras");
const statusEl = document.getElementById("status");
const refreshBtn = document.getElementById("refresh");

async function fetchCameras() {
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

    const status = document.createElement("span");
    status.className = `pill ${cam.publishing ? "online" : "offline"}`;
    status.textContent = cam.publishing ? "Publishing" : "Stopped";

    const toggle = document.createElement("button");
    toggle.textContent = cam.enabled ? "Disable" : "Enable";
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

    const actions = document.createElement("div");
    actions.className = "toggle";
    actions.append(status, toggle);

    card.append(info, actions);
    listEl.append(card);
  });
}

refreshBtn.addEventListener("click", fetchCameras);
fetchCameras();
setInterval(fetchCameras, 10000);
