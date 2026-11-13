// Recall UI: загрузка документов, чат со стримингом (SSE из fetch),
// источники под ответом.

const $ = (sel) => document.querySelector(sel);
const chat = $("#chat"), docs = $("#docs");

// --- Загрузка файлов ---

const input = $("#file");
$("#drop").addEventListener("click", () => input.click());
input.addEventListener("change", () => upload([...input.files]));

const drop = $("#drop");
["dragover", "dragenter"].forEach((ev) =>
    drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.add("over"); }));
["dragleave", "drop"].forEach((ev) =>
    drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.remove("over"); }));
drop.addEventListener("drop", (e) => upload([...e.dataTransfer.files]));

async function upload(files) {
    for (const f of files) {
        const fd = new FormData();
        fd.append("file", f);
        const r = await fetch("/api/documents", { method: "POST", body: fd });
        if (!r.ok) alert((await r.json()).error || r.status);
    }
    refresh();
}

async function refresh() {
    const list = await (await fetch("/api/documents")).json();
    docs.innerHTML = "";
    for (const d of list || []) {
        const li = document.createElement("li");
        li.innerHTML = `<span class="name">${d.name}</span>
			<span class="badge ${d.status}">${d.status}${d.error ? ": " + d.error : ""}</span>
			<button title="удалить">✕</button>`;
        li.querySelector("button").onclick = async () => {
            await fetch(`/api/documents/${d.id}`, { method: "DELETE" });
            refresh();
        };
        docs.appendChild(li);
    }
    const s = await (await fetch("/api/stats")).json();
    $("#stats").textContent = `${s.docs} док. · ${s.chunks} чанков`;
}

refresh();
setInterval(refresh, 5000); // лёгкий поллинг статусов обработки

// --- Чат: SSE из fetch-ридера ---
// EventSource умеет только GET, а нам нужен POST с JSON — поэтому
// читаем поток вручную: декодируем чанки, режем по "\n\n", разбираем.

let ctrl = null; // AbortController текущего запроса

$("#ask").addEventListener("submit", async (e) => {
    e.preventDefault();
    const question = $("#q").value.trim();
    if (!question || ctrl) return;
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
        if (!r.ok) throw new Error((await r.json()).error || r.status);

        const reader = r.body.getReader();
        const dec = new TextDecoder();
        let buf = "";

        while (true) {
            const { done, value } = await reader.read();
            if (done) break;
            buf += dec.decode(value, { stream: true });
            // Кадры SSE разделены пустой строкой; обрабатываем только полные.
            let idx;
            while ((idx = buf.indexOf("\n\n")) !== -1) {
                const frame = buf.slice(0, idx);
                buf = buf.slice(idx + 2);
                const ev = frame.match(/^event: (.+)$/m)?.[1];
                const data = frame.match(/^data: (.+)$/m)?.[1];
                if (!ev || data === undefined) continue;
                handle(ev, JSON.parse(data), reply);
            }
        }
    } catch (err) {
        if (err.name !== "AbortError") {
            reply.textContent += `\n\n⚠️ ${err.message}`;
        }
    } finally {
        reply.classList.remove("streaming");
        setBusy(false);
        ctrl = null;
    }
});

$("#stop").addEventListener("click", () => ctrl?.abort());

function handle(ev, data, reply) {
    if (ev === "token") reply.textContent += data;
    if (ev === "error") reply.textContent += `\n\n⚠️ ${data.error}`;
    if (ev === "sources") {
        const box = document.createElement("details");
        box.className = "sources";
        box.innerHTML = `<summary>Источники (${data.length})</summary>` +
            data.map((s) =>
                `<div class="src"><b>${s.doc}, фрагм. ${s.seq}</b> · релевантность ${s.score.toFixed(4)}<br>${s.text}</div>`)
                .join("");
        chat.insertBefore(box, reply);
    }
    if (ev === "done") box_ = null;
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

function setBusy(b) {
    $("#send").hidden = b;
    $("#stop").hidden = !b;
}
