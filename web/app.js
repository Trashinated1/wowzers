const audio = document.getElementById("audio");
let state = {
    queue: [],
    queueIndex: -1,
    track: null,
    player: "stopped",
    volume: 0.8,
    repeat: "off",
    shuffle: false,
    eqOn: false,
    eqPreset: "flat",
    sleepUntil: 0,
    sleepFade: false
};

let currentTab = "search";
let lastSearchResults = null;
let searchSubTab = "tracks";
let trackToAddToPl = null;
let currentBlobUrl = null;

// Web Audio variables
let audioCtx, sourceNode, gainNode, filterNodes = [];
let audioGraphInitialized = false; // FIX: Only initialize if EQ is turned ON

// ---------- iOS Audio Unlock Trick ----------
// iOS requires audio.play() to be called synchronously from a user gesture.
// Since we use `await` for caching, iOS loses the gesture context. 
// This silent unlock on the first tap fixes background playback.
let audioUnlocked = false;
const silentWav = "data:audio/wav;base64,UklGRiQAAABXQVZFZm10IBAAAAABAAEARKwAAIhYAQACABAAZGF0YQAAAAA=";
function unlockAudio() {
    if (audioUnlocked) return;
    const originalSrc = audio.src;
    audio.src = silentWav;
    audio.play().then(() => {
        audio.pause();
        audio.src = originalSrc;
        audioUnlocked = true;
    }).catch(() => {
        audio.src = originalSrc;
    });
}
document.addEventListener('touchstart', unlockAudio, { once: true });
document.addEventListener('click', unlockAudio, { once: true });

// ---------- IndexedDB Cache ----------
let db;
const dbReq = indexedDB.open("wowzers_cache", 1);
dbReq.onupgradeneeded = (e) => {
    db = e.target.result;
    if (!db.objectStoreNames.contains('tracks')) {
        db.createObjectStore('tracks', { keyPath: 'id' });
    }
};
dbReq.onsuccess = (e) => { db = e.target.result; };

function getCachedTrack(id) {
    return new Promise((resolve) => {
        if (!db) return resolve(null);
        const tx = db.transaction("tracks", "readonly");
        const store = tx.objectStore("tracks");
        const req = store.get(id);
        req.onsuccess = () => resolve(req.result ? req.result.blob : null);
        req.onerror = () => resolve(null);
    });
}

function cacheTrack(id, blob) {
    if (!db) return;
    const tx = db.transaction("tracks", "readwrite");
    const store = tx.objectStore("tracks");
    store.put({ id: id, blob: blob });
}

// ---------- Local Storage Helpers ----------
function getLocalFavs() { return JSON.parse(localStorage.getItem("wowzers_favorites") || "[]"); }
function isLocalFav(id) { return getLocalFavs().some(t => t.id === id); }
function toggleLocalFav(t) {
    let favs = getLocalFavs();
    if (isLocalFav(t.id)) {
        favs = favs.filter(x => x.id !== t.id);
    } else {
        favs.unshift(t);
    }
    localStorage.setItem("wowzers_favorites", JSON.stringify(favs));
}
function getLocalPlaylists() { return JSON.parse(localStorage.getItem("wowzers_playlists") || "{}"); }
function addToLocalPlaylist(name, track) {
    let pls = getLocalPlaylists();
    if (!pls[name]) pls[name] = [];
    if (!pls[name].some(t => t.id === track.id)) pls[name].push(track);
    localStorage.setItem("wowzers_playlists", JSON.stringify(pls));
}
function getLocalHistory() { return JSON.parse(localStorage.getItem("wowzers_history") || "[]"); }
function addToLocalHistory(t) {
    if (!t || !t.id) return;
    let hist = getLocalHistory();
    hist = hist.filter(x => x.id !== t.id);
    hist.unshift(t);
    if (hist.length > 100) hist = hist.slice(0, 100);
    localStorage.setItem("wowzers_history", JSON.stringify(hist));
}

// ---------- API ----------
async function api(path) {
    try {
        const r = await fetch(path, { credentials: "same-origin" });
        const text = await r.text();
        if (!r.ok) throw new Error(text || r.statusText || "Network error");
        try { return text ? JSON.parse(text) : {}; } catch (e) { return {}; }
    } catch (err) {
        console.error("API Error:", err);
        throw err;
    }
}

// ---------- Import / Export ----------
document.getElementById("exportBtn").onclick = () => {
    const data = { favorites: getLocalFavs(), playlists: getLocalPlaylists() };
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url; a.download = "wowzers_backup.json"; a.click();
    URL.revokeObjectURL(url);
};
document.getElementById("importBtn").onclick = () => document.getElementById("importFile").click();
document.getElementById("importFile").onchange = (e) => {
    const file = e.target.files[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = (event) => {
        try {
            const data = JSON.parse(event.target.result);
            if (data.favorites) localStorage.setItem("wowzers_favorites", JSON.stringify(data.favorites));
            if (data.playlists) localStorage.setItem("wowzers_playlists", JSON.stringify(data.playlists));
            alert("Imported successfully! Reloading...");
            location.reload();
        } catch (err) { alert("Invalid backup file."); }
    };
    reader.readAsText(file);
};

// ---------- Web Audio (FIX: Defer initialization unless EQ is ON) ----------
function setupAudioGraph() {
    if (audioGraphInitialized) return;
    try {
        audioCtx = new (window.AudioContext || window.webkitAudioContext)();
        sourceNode = audioCtx.createMediaElementSource(audio);
        gainNode = audioCtx.createGain();
        sourceNode.connect(gainNode).connect(audioCtx.destination);
        audioGraphInitialized = true;
    } catch (e) { console.warn("Web Audio init failed:", e); }
}
function applyEQ(preset, on) {
    if (!on) {
        // If the graph was previously initialized, bypass the filters
        if (audioGraphInitialized) {
            filterNodes.forEach(f => f.disconnect());
            filterNodes = [];
            if (sourceNode) {
                sourceNode.disconnect();
                gainNode.disconnect();
                sourceNode.connect(gainNode).connect(audioCtx.destination);
            }
        }
        return; 
    }
    
    setupAudioGraph(); // Initializes the graph only if EQ is turned ON
    if (!audioCtx) return;
    
    filterNodes.forEach(f => f.disconnect());
    filterNodes = [];
    if (sourceNode) sourceNode.disconnect();
    if (gainNode) gainNode.disconnect();
    
    const presets = { flat: [0,0,0,0,0,0], rock: [5,3,-1,-2,1,4], pop: [-1,2,3,2,0,-1], jazz: [3,2,-1,-1,1,3], classical: [4,3,0,-1,-1,2], bass: [8,6,2,0,0,0] };
    const bands = [60, 170, 350, 1000, 3500, 10000];
    const gains = presets[preset] || presets.flat;
    let prev = sourceNode;
    bands.forEach((freq, i) => {
        const f = audioCtx.createBiquadFilter();
        f.type = i === 0 ? "lowshelf" : (i === bands.length-1 ? "highshelf" : "peaking");
        f.frequency.value = freq; f.Q.value = 1; f.gain.value = gains[i];
        prev.connect(f); prev = f; filterNodes.push(f);
    });
    prev.connect(gainNode).connect(audioCtx.destination);
}

// ---------- Media Session API ----------
function setupMediaSession() {
    if (!('mediaSession' in navigator)) return;
    navigator.mediaSession.setActionHandler('play', () => playPause());
    navigator.mediaSession.setActionHandler('pause', () => playPause());
    navigator.mediaSession.setActionHandler('previoustrack', () => prev());
    navigator.mediaSession.setActionHandler('nexttrack', () => next());
    navigator.mediaSession.setActionHandler('seekto', (details) => {
        if (details.seekTime !== null) {
            audio.currentTime = details.seekTime;
        }
    });
}
function updateMediaSession() {
    if (!('mediaSession' in navigator)) return;
    const t = state.track;
    if (t) {
        navigator.mediaSession.metadata = new MediaMetadata({
            title: t.title, artist: t.artist, album: t.album,
            artwork: [{ src: t.cover || "/icon.svg", sizes: '512x512', type: 'image/jpeg' }]
        });
    }
    navigator.mediaSession.playbackState = state.player === "playing" ? "playing" : "paused";
}

// ---------- Core Player Logic ----------
async function playTrackAt(i) {
    if (i < 0 || i >= state.queue.length) {
        state.player = "stopped";
        state.track = null;
        updateUI();
        return;
    }
    
    state.queueIndex = i;
    state.track = state.queue[i];
    state.player = "playing";
    updateUI();
    addToLocalHistory(state.track);

    const id = state.track.id;
    
    // Check IndexedDB cache first
    let blob = await getCachedTrack(id);
    if (!blob) {
        try {
            const res = await fetch(`/api/stream/${id}`);
            if (!res.ok) throw new Error("Stream failed");
            blob = await res.blob();
            cacheTrack(id, blob); // Save to cache
        } catch (e) {
            console.error(e);
            alert("Failed to load track.");
            state.player = "stopped";
            updateUI();
            return;
        }
    }
    
    // Only play if the user hasn't skipped while we were loading/caching
    if (state.track && state.track.id === id) {
        if (currentBlobUrl) URL.revokeObjectURL(currentBlobUrl);
        currentBlobUrl = URL.createObjectURL(blob);
        audio.src = currentBlobUrl;
        audio.play().catch(() => {});
    }
}

function playPause() {
    if (!state.track) return;
    if (state.player === "playing") {
        state.player = "paused";
        audio.pause();
    } else {
        state.player = "playing";
        audio.play().catch(() => {});
    }
    updateUI();
}

function next() {
    if (state.repeat === "one") {
        audio.currentTime = 0;
        audio.play().catch(() => {});
        return;
    }
    let idx = state.queueIndex + 1;
    if (state.shuffle && state.queue.length > 1) {
        let randIdx;
        do { randIdx = Math.floor(Math.random() * state.queue.length); } while (randIdx === state.queueIndex);
        idx = randIdx;
    }
    if (idx >= state.queue.length) {
        if (state.repeat === "all" && state.queue.length > 0) idx = 0;
        else { state.player = "stopped"; updateUI(); return; }
    }
    playTrackAt(idx);
}

function prev() {
    if (audio.currentTime > 3) {
        audio.currentTime = 0;
        return;
    }
    let idx = state.queueIndex - 1;
    if (idx < 0) {
        if (state.repeat === "all" && state.queue.length > 0) idx = state.queue.length - 1;
        else idx = 0;
    }
    playTrackAt(idx);
}

function playTrackNow(t, queueContext) {
    let q = queueContext || [t];
    let idx = q.findIndex(x => x.id === t.id);
    if (idx === -1) {
        q = [t];
        idx = 0;
    }
    state.queue = q;
    playTrackAt(idx);
}

function addToQueue(t) {
    state.queue.push(t);
}

// ---------- Sleep Timer ----------
let sleepTimer = null;
let sleepFadeTimer = null;

function setSleep(minutes, fade) {
    if (sleepTimer) { clearTimeout(sleepTimer); sleepTimer = null; }
    if (sleepFadeTimer) { clearInterval(sleepFadeTimer); sleepFadeTimer = null; }
    
    if (minutes <= 0) {
        state.sleepUntil = 0;
        state.sleepFade = false;
        audio.volume = state.volume; // restore volume
        updateUI();
        return;
    }
    
    state.sleepUntil = Date.now() + minutes * 60000;
    state.sleepFade = fade;
    updateUI();
    
    sleepTimer = setTimeout(() => {
        if (fade) {
            let steps = 12;
            let i = 1;
            sleepFadeTimer = setInterval(() => {
                if (i > steps) {
                    clearInterval(sleepFadeTimer);
                    state.player = "paused";
                    state.sleepUntil = 0;
                    state.sleepFade = false;
                    audio.volume = state.volume;
                    audio.pause();
                    updateUI();
                    return;
                }
                audio.volume = state.volume * (steps - i) / steps;
                i++;
            }, 500);
        } else {
            state.player = "paused";
            state.sleepUntil = 0;
            audio.pause();
            updateUI();
        }
    }, minutes * 60000);
}

// ---------- UI Updater ----------
function updateUI() {
    const t = state.track;
    const coverUrl = t ? (t.cover || "/icon.svg") : "/icon.svg";
    const title = t ? t.title : "Nothing playing";
    const artist = t ? t.artist : "—";
    const dur = t ? fmt(t.durationMs) : "0:00";
    const playIcon = state.player === "playing" ? "⏸" : "▶";

    document.getElementById("playerTitle").textContent = title;
    document.getElementById("playerArtist").textContent = artist;
    document.getElementById("playerCover").src = coverUrl;
    document.getElementById("durLabel").textContent = dur;
    document.getElementById("playBtn").textContent = playIcon;
    
    document.getElementById("npTitle").textContent = title;
    document.getElementById("npArtist").textContent = artist;
    document.getElementById("npCover").src = coverUrl;
    document.getElementById("npDurLabel").textContent = dur;
    document.getElementById("npPlayBtn").textContent = playIcon;

    // Setup seek bars
    const seek = document.getElementById("seek");
    const npSeek = document.getElementById("npSeek");
    if (seek) seek.max = t ? t.durationMs : 1000;
    if (npSeek) npSeek.max = t ? t.durationMs : 1000;

    const isFav = t ? isLocalFav(t.id) : false;
    const npFavBtn = document.getElementById("npFavBtn");
    if (npFavBtn) {
        npFavBtn.textContent = isFav ? '♥' : '♡';
        npFavBtn.classList.toggle("active", isFav);
    }

    document.getElementById("shuffleBtn").classList.toggle("active", state.shuffle);
    document.getElementById("repeatBtn").classList.toggle("active", state.repeat !== "off");
    document.getElementById("repeatBtn").textContent = state.repeat === "one" ? "↻¹" : "↻";
    document.getElementById("npShuffleBtn").classList.toggle("active", state.shuffle);
    document.getElementById("npRepeatBtn").classList.toggle("active", state.repeat !== "off");
    document.getElementById("npRepeatBtn").textContent = state.repeat === "one" ? "↻¹" : "↻";

    document.getElementById("eqOn").checked = state.eqOn;
    document.getElementById("eqPreset").value = state.eqPreset;
    applyEQ(state.eqPreset, state.eqOn);

    const ss = document.getElementById("sleepStatus");
    if (state.sleepUntil) {
        const mins = Math.max(0, Math.round((state.sleepUntil - Date.now()) / 60000));
        ss.textContent = `Sleeping in ${mins} min${state.sleepFade ? " (fade)" : ""}.`;
    } else { ss.textContent = ""; }

    audio.volume = state.volume;
    document.getElementById("volume").value = Math.round(state.volume * 100);

    updateMediaSession();
    if (currentTab === "queue") loadQueue();
}

function fmt(ms) { if (!ms || ms < 0) return "0:00"; const s = Math.floor(ms / 1000); return Math.floor(s / 60) + ":" + String(s % 60).padStart(2, "0"); }

// ---------- Event Listeners ----------
document.getElementById("playerTrackInfo").onclick = () => document.getElementById("nowPlayingScreen").classList.remove("hidden");
document.getElementById("npClose").onclick = () => document.getElementById("nowPlayingScreen").classList.add("hidden");

document.getElementById("npFavBtn").onclick = () => {
    if (!state.track) return;
    toggleLocalFav(state.track);
    updateUI();
};
document.getElementById("npPlBtn").onclick = () => {
    if (!state.track) return;
    trackToAddToPl = state.track;
    openPlaylistModal();
};

function bindTransport(btnId, fn) {
    const btn = document.getElementById(btnId);
    if (btn) btn.onclick = fn;
}
bindTransport("playBtn", playPause);
bindTransport("nextBtn", next);
bindTransport("prevBtn", prev);
bindTransport("npPlayBtn", playPause);
bindTransport("npNextBtn", next);
bindTransport("npPrevBtn", prev);

document.getElementById("shuffleBtn").onclick = () => { state.shuffle = !state.shuffle; updateUI(); };
document.getElementById("repeatBtn").onclick = () => {
    if (state.repeat === "off") state.repeat = "all";
    else if (state.repeat === "all") state.repeat = "one";
    else state.repeat = "off";
    updateUI();
};
document.getElementById("npShuffleBtn").onclick = () => { state.shuffle = !state.shuffle; updateUI(); };
document.getElementById("npRepeatBtn").onclick = () => {
    if (state.repeat === "off") state.repeat = "all";
    else if (state.repeat === "all") state.repeat = "one";
    else state.repeat = "off";
    updateUI();
};

const volEl = document.getElementById("volume");
volEl.oninput = () => { state.volume = volEl.value / 100; audio.volume = state.volume; };

// ---------- Seek Bar Logic ----------
let seekDragging = false;

function bindSeek(elId, posId) {
    const el = document.getElementById(elId);
    if (!el) return;
    
    el.addEventListener('pointerdown', () => seekDragging = true);
    
    el.addEventListener('input', () => {
        document.getElementById(posId).textContent = fmt(parseInt(el.value));
    });
    
    el.addEventListener('pointerup', () => {
        if (seekDragging) {
            audio.currentTime = parseInt(el.value) / 1000;
            setTimeout(() => seekDragging = false, 500);
        }
    });
}

window.addEventListener('pointerup', () => {
    if (seekDragging) {
        setTimeout(() => seekDragging = false, 500);
    }
});

bindSeek("seek", "posLabel");
bindSeek("npSeek", "npPosLabel");

audio.addEventListener("ended", next);

audio.addEventListener("timeupdate", () => {
    const v = Math.floor(audio.currentTime * 1000);
    document.getElementById("posLabel").textContent = fmt(v);
    document.getElementById("npPosLabel").textContent = fmt(v);
    
    if (!seekDragging) {
        const seek = document.getElementById("seek");
        const npSeek = document.getElementById("npSeek");
        if (seek) seek.value = v;
        if (npSeek) npSeek.value = v;
    }
});

// ---------- Nav ----------
document.querySelectorAll(".nav button").forEach(b => b.onclick = () => switchTab(b.dataset.tab));
function switchTab(tab) {
    currentTab = tab;
    document.querySelectorAll(".nav button").forEach(b => b.classList.toggle("active", b.dataset.tab === tab));
    document.getElementById("contentTitle").textContent = tab.charAt(0).toUpperCase() + tab.slice(1);
    document.getElementById("content").innerHTML = "";
    if (tab === "search" && lastSearchResults) renderSearch(lastSearchResults);
    if (tab === "charts") loadCharts();
    if (tab === "local_fav") loadLocalFavs();
    if (tab === "local_pl") loadLocalPlaylists();
    if (tab === "queue") loadQueue();
    if (tab === "history") loadHistory();
}

const searchInput = document.getElementById("searchInput");
let searchTimer;
searchInput.oninput = () => {
    clearTimeout(searchTimer);
    const q = searchInput.value.trim();
    if (!q) return;
    searchTimer = setTimeout(async () => {
        try {
            const res = await api("/api/search?q=" + encodeURIComponent(q));
            lastSearchResults = res;
            if (currentTab === "search") renderSearch(res);
        } catch (e) { console.error(e); }
    }, 300);
};
searchInput.onkeydown = (e) => { if (e.key === "Enter") searchInput.oninput(); };

function renderSearch(res) {
    const c = document.getElementById("content");
    c.innerHTML = "";
    
    const subNav = document.createElement("div");
    subNav.className = "sub-nav";
    subNav.innerHTML = `
        <button data-sub="tracks" class="active">Tracks</button>
        <button data-sub="albums">Albums (NOT WORKING)</button>
        <button data-sub="artists">Artists (NOT WORKING)</button>
    `;
    c.appendChild(subNav);
    
    const listContainer = document.createElement("div");
    listContainer.id = "searchListContainer";
    c.appendChild(listContainer);
    
    subNav.querySelectorAll("button").forEach(btn => {
        btn.onclick = () => {
            subNav.querySelectorAll("button").forEach(b => b.classList.remove("active"));
            btn.classList.add("active");
            searchSubTab = btn.dataset.sub;
            renderSearchList(res, searchSubTab);
        };
    });
    renderSearchList(res, searchSubTab);
}

function renderSearchList(res, sub) {
    const container = document.getElementById("searchListContainer");
    if (!container) return;
    container.innerHTML = "";
    
    if (sub === "tracks" && res.tracks && res.tracks.length) {
        const list = document.createElement("div"); list.className = "list";
        res.tracks.forEach(t => list.appendChild(trackRow(t, res.tracks)));
        container.appendChild(list);
    } else if (sub === "albums" && res.albums && res.albums.length) {
        const grid = document.createElement("div"); grid.className = "grid";
        res.albums.forEach(a => grid.appendChild(albumCard(a)));
        container.appendChild(grid);
    } else if (sub === "artists" && res.artists && res.artists.length) {
        const grid = document.createElement("div"); grid.className = "grid";
        res.artists.forEach(a => grid.appendChild(artistCard(a)));
        container.appendChild(grid);
    } else {
        container.innerHTML = `<p class="hint">No results found.</p>`;
    }
}

function trackRow(t, queueContext, queueIdx) {
    const el = document.createElement("div");
    el.className = "list-row";
    if (state.track && state.track.id === t.id) el.classList.add("active");
    
    const isFav = isLocalFav(t.id);
    el.innerHTML = `
        <img src="${t.cover || "/icon.svg"}" alt="">
        <div class="info"><div class="t">${escapeHtml(t.title)}</div><div class="s">${escapeHtml(t.artist)}</div></div>
        <button class="action-btn fav-btn ${isFav ? 'active' : ''}" title="Favorite">${isFav ? '♥' : '♡'}</button>
        <button class="action-btn add-pl-btn" title="Add to Playlist">📚</button>
        <div class="dur">${fmt(t.durationMs)}</div>`;
        
    el.querySelector(".fav-btn").onclick = (e) => {
        e.stopPropagation();
        toggleLocalFav(t);
        const nowFav = isLocalFav(t.id);
        e.target.textContent = nowFav ? '♥' : '♡';
        e.target.classList.toggle('active', nowFav);
    };
    
    el.querySelector(".add-pl-btn").onclick = (e) => {
        e.stopPropagation();
        trackToAddToPl = t;
        openPlaylistModal();
    };
    
    el.onclick = () => {
        if (queueIdx !== undefined && queueIdx !== null) {
            playTrackAt(queueIdx);
        } else {
            playTrackNow(t, queueContext);
        }
    };
    return el;
}

function albumCard(a) {
    const el = document.createElement("div");
    el.className = "card";
    el.innerHTML = `<img src="${a.cover || "/icon.svg"}" alt=""><div class="title">${escapeHtml(a.title)}</div><div class="subtitle">${a.nbTracks || ""} tracks</div>`;
    el.onclick = () => loadAlbum(a.id);
    return el;
}

function artistCard(a) {
    const el = document.createElement("div");
    el.className = "card";
    el.innerHTML = `<img src="${a.cover || "/icon.svg"}" alt=""><div class="title">${escapeHtml(a.name)}</div><div class="subtitle">${a.nbFans || 0} fans</div>`;
    el.onclick = async () => {
        const page = await api("/api/artist/" + a.id);
        document.getElementById("contentTitle").textContent = page.artist.name;
        const c = document.getElementById("content"); c.innerHTML = "";
        if (page.topTracks && page.topTracks.length) {
            const sec = document.createElement("div");
            sec.innerHTML = `<h3 style="margin-bottom:16px;">Top tracks</h3>`;
            const list = document.createElement("div"); list.className = "list";
            page.topTracks.forEach(t => list.appendChild(trackRow(t, page.topTracks)));
            sec.appendChild(list); c.appendChild(sec);
        }
        if (page.albums && page.albums.length) {
            const sec = document.createElement("div"); sec.className = "section-gap";
            sec.innerHTML = `<h3 style="margin-bottom:16px;">Albums</h3>`;
            const grid = document.createElement("div"); grid.className = "grid";
            page.albums.forEach(al => grid.appendChild(albumCard(al)));
            sec.appendChild(grid); c.appendChild(sec);
        }
    };
    return el;
}

function escapeHtml(s) { return String(s || "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }

// ---------- Charts, Albums, Playlists (Deezer) UI ----------
async function loadCharts() {
    const res = await api("/api/charts");
    const c = document.getElementById("content"); c.innerHTML = "";
    const list = document.createElement("div"); list.className = "list";
    (res.tracks || []).forEach(t => list.appendChild(trackRow(t, res.tracks)));
    c.appendChild(list);
}

async function loadAlbum(id) {
    const data = await api("/api/album/" + id);
    document.getElementById("contentTitle").textContent = data.title || "Album";
    const c = document.getElementById("content");
    c.innerHTML = "";
    
    const view = document.createElement("div");
    view.className = "album-view";
    
    const header = document.createElement("div");
    header.className = "album-header";
    header.innerHTML = `
        <img src="${data.cover || "/icon.svg"}" alt="">
        <div class="album-meta">
            <h1>${escapeHtml(data.title)}</h1>
            <p>${escapeHtml(data.artist)} • ${data.nbTracks || (data.tracks || []).length} tracks</p>
            <button class="album-play-btn">Play Album</button>
        </div>
    `;
    header.querySelector(".album-play-btn").onclick = () => playTrackNow(data.tracks[0], data.tracks);
    view.appendChild(header);
    
    const list = document.createElement("div");
    list.className = "list";
    data.tracks.forEach(t => list.appendChild(trackRow(t, data.tracks)));
    view.appendChild(list);
    
    c.appendChild(view);
}

// ---------- Local Favorites & Playlists UI ----------
function loadLocalFavs() {
    const tracks = getLocalFavs();
    const c = document.getElementById("content"); c.innerHTML = "";
    document.getElementById("contentTitle").textContent = "Local Favorites";
    if (tracks.length === 0) {
        c.innerHTML = `<p class="hint">No favorites yet. Click the ♡ button on tracks to add them here.</p>`;
        return;
    }
    const list = document.createElement("div"); list.className = "list";
    tracks.forEach(t => list.appendChild(trackRow(t, tracks)));
    c.appendChild(list);
}

function loadLocalPlaylists() {
    const pls = getLocalPlaylists();
    const c = document.getElementById("content"); c.innerHTML = "";
    document.getElementById("contentTitle").textContent = "Local Playlists";
    
    if (Object.keys(pls).length === 0) {
        c.innerHTML = `<p class="hint">No playlists yet. Click the 📚 button on tracks to create one.</p>`;
        return;
    }
    
    const grid = document.createElement("div"); grid.className = "grid";
    Object.keys(pls).forEach(name => {
        const el = document.createElement("div"); el.className = "card";
        const firstTrack = pls[name][0];
        el.innerHTML = `<img src="${firstTrack?.cover || "/icon.svg"}" alt=""><div class="title">${escapeHtml(name)}</div><div class="subtitle">${pls[name].length} tracks</div>`;
        el.onclick = () => loadLocalPlaylist(name);
        grid.appendChild(el);
    });
    c.appendChild(grid);
}

async function loadLocalPlaylist(name) {
    const pls = getLocalPlaylists();
    const tracks = pls[name] || [];
    document.getElementById("contentTitle").textContent = name;
    const c = document.getElementById("content"); c.innerHTML = "";
    
    const view = document.createElement("div");
    view.className = "album-view";
    
    const header = document.createElement("div");
    header.className = "album-header";
    header.innerHTML = `
        <img src="${tracks[0]?.cover || "/icon.svg"}" alt="">
        <div class="album-meta">
            <h1>${escapeHtml(name)}</h1>
            <p>Local Playlist • ${tracks.length} tracks</p>
            <div class="playlist-actions">
                <button class="album-play-btn">Play</button>
                <button class="album-play-btn shuffle-btn">Shuffle</button>
            </div>
        </div>
    `;
    
    header.querySelector(".album-play-btn").onclick = () => playTrackNow(tracks[0], tracks);
    header.querySelector(".shuffle-btn").onclick = () => {
        state.shuffle = true;
        playTrackNow(tracks[0], tracks);
        setTimeout(() => { state.shuffle = false; updateUI(); }, 1000);
    };
    view.appendChild(header);
    
    const list = document.createElement("div");
    list.className = "list";
    tracks.forEach(t => list.appendChild(trackRow(t, tracks)));
    view.appendChild(list);
    
    c.appendChild(view);
}

// ---------- Playlist Modal ----------
function openPlaylistModal() {
    const modal = document.getElementById("playlistModal");
    const list = document.getElementById("localPlList");
    list.innerHTML = "";
    const pls = getLocalPlaylists();
    const names = Object.keys(pls);
    
    if (names.length === 0) {
        list.innerHTML = `<p class="hint">No local playlists yet. Create one below.</p>`;
    } else {
        names.forEach(name => {
            const btn = document.createElement("button");
            btn.className = "pl-item";
            btn.textContent = `${name} (${pls[name].length} tracks)`;
            btn.onclick = () => {
                addToLocalPlaylist(name, trackToAddToPl);
                alert(`Added to ${name}!`);
                modal.classList.add("hidden");
            };
            list.appendChild(btn);
        });
    }
    modal.classList.remove("hidden");
}

document.getElementById("createPlBtn").onclick = () => {
    const name = document.getElementById("newPlName").value.trim();
    if (!name) return;
    addToLocalPlaylist(name, trackToAddToPl);
    alert(`Created ${name} and added track!`);
    document.getElementById("playlistModal").classList.add("hidden");
    document.getElementById("newPlName").value = "";
};

// ---------- Queue & History ----------
function loadQueue() {
    const c = document.getElementById("content");
    document.getElementById("contentTitle").textContent = "Play Queue";
    c.innerHTML = "";
    if (!state.queue || state.queue.length === 0) {
        c.innerHTML = `<p class="hint">Queue is empty. Play an album or click the '+' button on a track.</p>`;
        return;
    }
    const list = document.createElement("div");
    list.className = "list";
    state.queue.forEach((t, i) => {
        const el = document.createElement("div");
        el.className = "list-row";
        if (i === state.queueIndex) el.classList.add("active");
        el.innerHTML = `
            <img src="${t.cover || "/icon.svg"}" alt="">
            <div class="info"><div class="t">${escapeHtml(t.title)}</div><div class="s">${escapeHtml(t.artist)}</div></div>
            <button class="action-btn" title="Remove" style="border-color: var(--danger); color: var(--danger);">✕</button>
            <div class="dur">${fmt(t.durationMs)}</div>`;
        el.querySelector(".action-btn").onclick = (e) => {
            e.stopPropagation();
            state.queue.splice(i, 1);
            if (i < state.queueIndex) state.queueIndex--;
            loadQueue();
        };
        el.onclick = () => playTrackAt(i);
        list.appendChild(el);
    });
    c.appendChild(list);
}

function loadHistory() {
    const tracks = getLocalHistory();
    const c = document.getElementById("content"); c.innerHTML = "";
    document.getElementById("contentTitle").textContent = "History";
    if (tracks.length === 0) {
        c.innerHTML = `<p class="hint">No history yet. Play a track to see it here.</p>`;
        return;
    }
    const list = document.createElement("div"); list.className = "list";
    tracks.forEach(t => list.appendChild(trackRow(t)));
    c.appendChild(list);
}

// ---------- Overlays ----------
document.getElementById("lyricsBtn").onclick = async () => {
    toggleOverlay("lyricsPanel");
    if (!state.track) return;
    try {
        const lyr = await api("/api/lyrics/" + state.track.id);
        const body = document.getElementById("lyricsBody");
        body.innerHTML = "";
        if (lyr.synced && lyr.synced.length) {
            lyr.synced.forEach(l => {
                const el = document.createElement("div");
                el.className = "line"; el.dataset.time = l.timeMs; el.textContent = l.text;
                body.appendChild(el);
            });
        } else if (lyr.unsynced && lyr.unsynced.length) {
            lyr.unsynced.forEach(t => {
                const el = document.createElement("div");
                el.className = "line"; el.textContent = t; body.appendChild(el);
            });
        } else { body.innerHTML = `<p class="hint">No lyrics found.</p>`; }
    } catch (e) { document.getElementById("lyricsBody").innerHTML = `<p class="hint">${e.message}</p>`; }
};
document.getElementById("eqBtn").onclick = () => toggleOverlay("eqPanel");
document.getElementById("sleepBtn").onclick = () => toggleOverlay("sleepPanel");
document.querySelectorAll("[data-close]").forEach(b => b.onclick = () => b.closest(".overlay").classList.add("hidden"));
function toggleOverlay(id) {
    document.querySelectorAll(".overlay").forEach(o => { if (o.id !== id) o.classList.add("hidden"); });
    document.getElementById(id).classList.toggle("hidden");
}

setInterval(() => {
    if (document.getElementById("lyricsPanel").classList.contains("hidden")) return;
    if (!state.track) return;
    const pos = audio.currentTime * 1000;
    const lines = document.querySelectorAll("#lyricsBody .line");
    let active = null;
    lines.forEach(l => {
        const t = parseInt(l.dataset.time);
        if (!isNaN(t) && t <= pos) active = l;
    });
    document.querySelectorAll("#lyricsBody .line.active").forEach(l => l.classList.remove("active"));
    if (active) {
        active.classList.add("active");
        active.scrollIntoView({ block: "center", behavior: "smooth" });
    }
}, 500);

document.getElementById("eqOn").onchange = (e) => { state.eqOn = e.target.checked; updateUI(); };
document.getElementById("eqPreset").onchange = (e) => { state.eqPreset = e.target.value; updateUI(); };

document.querySelectorAll(".sleep-presets button").forEach(b => b.onclick = () => {
    const fade = document.getElementById("sleepFade").checked;
    setSleep(parseInt(b.dataset.min), fade);
});
document.getElementById("sleepCancel").onclick = () => setSleep(0, false);

if ("serviceWorker" in navigator) { navigator.serviceWorker.register("/sw.js").catch(console.warn); }

// ---------- bootstrap ----------
setupMediaSession();
updateUI();
fetch("/api/status").then(r => {
    if (!r.ok) {
        document.getElementById("content").innerHTML = `<p class="hint">Search for something lol</p>`;
    }
}).catch(() => {});