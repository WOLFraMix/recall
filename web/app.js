// Recall UI: загрузка документов, чат со стримингом (SSE из fetch).
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
