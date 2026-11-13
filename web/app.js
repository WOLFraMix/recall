// Recall UI: загрузка документов, чат со стримингом (SSE из fetch), поиск.
// Правила надёжности:
//  1) состояние занятости — отдельный флаг busy, восстанавливается в finally;
//  2) один битый кадр потока не убивает обработку остальных (try/catch в цикле);
//  3) всё, что попадает в DOM из данных сервера — через textContent,
//     никогда через innerHTML (иначе имена файлов с HTML-кодом выполняются).

const $ = (sel) => document.querySelector(sel);
const chat = $("#chat"), docs = $("#docs");

let busy = false;
let ctrl = null;

function setBusy(b) {
    busy = b;
    $("#send").hidden = b;
    $("#stop").hidden = !b;
}

// esc — экранирование произвольного текста перед вставкой в HTML.
function esc(s) {
    const d = document.createElement("div");
    d.textContent = s == null ? "" : String(s);
    return d.innerHTML;
}

// --- Вкладки ---

const tabs = document.querySelectorAll(".tab");
tabs.forEach((t) => t.addEventListener("click", () => {
    tabs.forEach((x) => x.classList.toggle("active", x === t));
    document.getElementById("view-chat").hidden = t.dataset.tab !== "chat";
    document.getElementById("view-search").hidden = t.dataset.tab !== "search";
}));

// --- Загрузка файлов ---

const input = $("#file");
$("#drop").addEventListener("click", () => input.click());
input.addEventListener("change", () => upload([...input.files]));

const drop = $("#drop");
["dragover", "dragenter"].forEach((ev) =>
    drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.add("over"); }));
["dragleave", "drop"].forEach((ev) =>
    drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.remove("over"); }));
drop.addEventListener("drop", (e) => { e.preventDefault(); drop.classList.remove("over"); upload([...e.dataTransfer.files]); });

async function upload(files) {
    for (const f of files) {
        const fd = new FormData();
        fd.append("file", f);
        try {
            const r = await fetch("/api/documents", { method: "POST", body: fd });
            if (!r.ok) {
                const j = await r.json().catch(() => ({}));
                alert(j.error || `ошибка ${r.status}`);
            }
        } catch (e) {
            alert("сеть недоступна: " + e.message);
        }
    }
    refresh();
}

async function refresh() {
    try {
        const r = await fetch("/api/documents");
        if (!r.ok) return; // поллинг не должен сыпать ошибками в консоль
        const list = await r.json();
        docs.innerHTML = "";
        for (const d of Array.isArray(list) ? list : []) {
            const li = document.createElement("li");

            const name = document.createElement("span");
            name.className = "name";
            name.textContent = d.name; // безопасно при любом имени файла

            const badge = document.createElement("span");
            badge.className = `badge ${esc(d.status)}`;
            badge.textContent = d.status + (d.error ? ": " + d.error : "");

            const del = document.createElement("button");
            del.textContent = "✕";
            del.title = "удалить";
            del.onclick = async () => {
                await fetch(`/api/documents/${d.id}`, { method: "DELETE" });
                refresh();
            };

            li.append(name, badge, del);
            docs.appendChild(li);
        }
        const s = await (await fetch("/api/stats")).json();
        $("#stats").textContent = `${s.docs} док. · ${s.chunks} чанков`;
    } catch (e) {
        // молча: поллинг живёт своей жизнью
    }
}

refresh();
setInterval(refresh, 5000);

// --- Чат ---

$("#ask").addEventListener("submit", (e) => { e.preventDefault(); ask(); });
$("#stop").addEventListener("click", () => { if (ctrl) ctrl.abort(); });

async function ask() {
    if (busy) return;                  // защита от двойного сабмита
    const question = $("#q").value.trim();
    if (!question) return;
    $("#q").value = "";

    addMsg("user", question);
    const reply = addMsg("assistant", "");
    reply.classList.add("streaming");
    setBusy(true);
    ctrl = new AbortController();

    try {
        const r = await fetch("/api/chat", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ question }),
            signal: ctrl.signal,
        });
        if (!r.ok) {
            const j = await r.json().catch(() => ({}));
            throw new Error(j.error || `ошибка ${r.status}`);
        }

        const reader = r.body.getReader();
        const dec = new TextDecoder();
        let buf = "";

        while (true) {
            const { done, value } = await reader.read();
            if (done) break;
            buf += dec.decode(value, { stream: true });
            let idx;
            while ((idx = buf.indexOf("\n\n")) !== -1) {
                const frame = buf.slice(0, idx);
                buf = buf.slice(idx + 2);
                const ev = /^event: (.+)$/m.exec(frame)?.[1];
                const raw = /^data: (.+)$/m.exec(frame)?.[1];
                if (!ev || raw === undefined) continue;
                try {
                    handle(ev, JSON.parse(raw), reply);
                } catch (err) {
                    // один битый кадр — не повод рвать весь поток
                    console.error("bad SSE frame:", frame, err);
                }
            }
        }
    } catch (err) {
        if (err.name !== "AbortError") {
            reply.textContent += `\n\n⚠️ ${err.message}`;
        }
    } finally {
        // Выполняется ВСЕГДА: и при ошибке, и при «Стоп», и при обрыве сети.
        // Кнопки не могут залипнуть по построению.
        reply.classList.remove("streaming");
        setBusy(false);
        ctrl = null;
    }
}

function handle(ev, data, reply) {
    if (ev === "token") reply.textContent += data;
    if (ev === "error") reply.textContent += `\n\n⚠️ ${data.error}`;
    if (ev === "sources" && Array.isArray(data)) {
        const box = document.createElement("details");
        box.className = "sources";
        const sum = document.createElement("summary");
        sum.textContent = `Источники (${data.length})`;
        box.appendChild(sum);
        for (const s of data) {
            const div = document.createElement("div");
            div.className = "src";
            const head = document.createElement("b");
            head.textContent = `${s.doc}, фрагм. ${s.seq} · релевантность ${Number(s.score).toFixed(4)}`;
            div.appendChild(head);
            div.appendChild(document.createTextNode(s.text));
            box.appendChild(div);
        }
        chat.insertBefore(box, reply);
    }
    chat.scrollTop = chat.scrollHeight;
}

function addMsg(role, text) {
    const div = document.createElement("div");
    div.className = `msg ${role}`;
    div.textContent = text;
    chat.appendChild(div);
    chat.scrollTop = chat.scrollHeight;
    return div;
}

// --- Поиск ---

async function doSearch() {
    const q = $("#sq").value.trim();
    if (!q) return;
    const box = $("#results");
    box.replaceChildren();

    try {
        const r = await fetch(`/api/search?q=${encodeURIComponent(q)}`);
        if (!r.ok) throw new Error(`ошибка ${r.status}`);
        const hits = await r.json();
        if (!Array.isArray(hits) || !hits.length) {
            box.textContent = "Ничего не найдено";
            return;
        }
        // RRF-оценки — не проценты (это суммы ранговых весов ~1/(60+rank)),
        // поэтому шкала относительная: лучший хит = 100%.
        const max = Math.max(...hits.map((h) => Number(h.score) || 0), 1e-9);
        for (const h of hits) {
            const card = document.createElement("div");
            card.className = "hit";

            const head = document.createElement("div");
            head.className = "hit-head";
            const doc = document.createElement("b");
            doc.textContent = h.doc ?? "";
            const meta = document.createElement("span");
            meta.className = "hit-meta";
            meta.textContent = `фрагм. ${h.seq}`;
            const bar = document.createElement("div");
            bar.className = "bar";
            const fill = document.createElement("div");
            fill.className = "fill";
            fill.style.width = Math.round(((Number(h.score) || 0) / max) * 100) + "%";
            bar.appendChild(fill);
            head.append(doc, meta, bar);

            const text = document.createElement("p");
            text.textContent = h.text ?? "";
            card.append(head, text);
            box.appendChild(card);
        }
    } catch (e) {
        box.textContent = "⚠️ " + e.message;
    }
}

$("#sbtn").addEventListener("click", doSearch);
$("#sq").addEventListener("keydown", (e) => { if (e.key === "Enter") doSearch(); });
