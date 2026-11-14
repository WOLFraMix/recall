// Recall UI: загрузка документов, чат со стримингом (SSE из fetch), поиск, выжимка.
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
//
// Универсальный переключатель: работает для любой вкладки, добавленной
// в index.html с data-tab и парной панелью <main id="view-имя">.
// Новая вкладка больше не требует правок в JS — только HTML.
const tabs = document.querySelectorAll(".tab");
tabs.forEach((t) => t.addEventListener("click", () => {
    tabs.forEach((x) => x.classList.toggle("active", x === t));
    document.querySelectorAll("main[id^='view-']").forEach((v) => {
        v.hidden = v.id !== "view-" + t.dataset.tab;
    });
    // Список документов выжимки обновляем при входе на вкладку:
    // пользователь мог залить файл с прошлой вкладки.
    if (t.dataset.tab === "summarize") refreshSumDocs();
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
        // Нормализация полей: бэкенд мог отдать их в нижнем регистре
        // (с json-тегами) или как есть из Go ("Doc", "Seq"...).
        // Принимаем оба варианта — фронтенд не зависит от нейминга.
        const norm = hits.map((h) => ({
            doc: h.doc ?? h.Doc ?? "",
            seq: h.seq ?? h.Seq ?? "",
            text: h.text ?? h.Text ?? "",
            score: Number(h.score ?? h.Score ?? 0),
        }));
        // RRF-оценки — не проценты (это суммы ранговых весов ~1/(60+rank)),
        // поэтому шкала относительная: лучший хит = 100%.
        const max = Math.max(...norm.map((h) => h.score), 1e-9);
        for (const h of norm) {
            const card = document.createElement("div");
            card.className = "hit";

            const head = document.createElement("div");
            head.className = "hit-head";
            const doc = document.createElement("b");
            doc.textContent = h.doc;
            const meta = document.createElement("span");
            meta.className = "hit-meta";
            meta.textContent = h.seq !== "" ? `фрагм. ${h.seq}` : "";
            const bar = document.createElement("div");
            bar.className = "bar";
            const fill = document.createElement("div");
            fill.className = "fill";
            fill.style.width = Math.round((h.score / max) * 100) + "%";
            bar.appendChild(fill);
            head.append(doc, meta, bar);

            const text = document.createElement("p");
            highlightTerms(text, h.text, q);
            card.append(head, text);
            box.appendChild(card);
        }
    } catch (e) {
        box.textContent = "⚠️ " + e.message;
    }
}

// highlightTerms — подсветка совпадений запроса в тексте фрагмента.
// Те же основы, что строит FTS-канал (псевдо-стемминг): слово длиннее
// 4 букв теряет последнюю букву, короткие ищутся целиком. Так подсветка
// согласована с поиском: подсвечивается ровно то, по чему фрагмент
// нашёлся — включая шум вроде «машинально» по запросу «машина».
// Текст собирается через DOM-узлы, а не innerHTML — пользовательский
// текст не может стать исполняемым HTML (XSS-безопасность).
function highlightTerms(el, text, query) {
    const stems = [...new Set(
        query.toLowerCase()
            .split(/\s+/)
            .map((w) => w.replace(/[^\p{L}\p{N}]/gu, ""))
            .filter((w) => w.length > 0)
            .map((w) => (w.length > 4 ? w.slice(0, -1) : w))
    )];
    if (!stems.length) {
        el.textContent = text;
        return;
    }
    // Более длинные основы в приоритете: «дариэн» не должна перехватываться
    // более коротким совпадением. Спецсимволы экранируем — основа может
    // содержать всё что угодно после чистки пунктуации не всегда чиста.
    const esc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    const re = new RegExp(
        "(" + stems.sort((a, b) => b.length - a.length).map(esc).join("|") + ")",
        "giu"
    );

    let last = 0, m;
    while ((m = re.exec(text)) !== null) {
        if (m[0].length === 0) { re.lastIndex++; continue; } // страховка от зацикливания
        el.append(text.slice(last, m.index));
        const mark = document.createElement("mark");
        mark.textContent = m[0];
        el.append(mark);
        last = m.index + m[0].length;
    }
    el.append(text.slice(last));
}

$("#sbtn").addEventListener("click", doSearch);
$("#sq").addEventListener("keydown", (e) => { if (e.key === "Enter") doSearch(); });

// --- Выжимка ---
//
// Принцип после бага с «Готово» на пустом статусе: фронтенд НИКОГДА
// не догадывается о состоянии задачи — он читает его из базы. Единственная
// точка синхронизации — applySummaryState; её зовут при входе на вкладку,
// смене документа, после 409 и по таймеру поллинга.

const sumRun = $("#sum-run");
const sumStatus = $("#sum-status");
let sumPolling = null; // id документа, который мы сейчас поллим

async function refreshSumDocs() {
    try {
        const r = await fetch("/api/documents");
        if (!r.ok) return;
        const list = await r.json();
        const sel = $("#sum-doc");
        const prev = sel.value;
        sel.replaceChildren();
        for (const d of Array.isArray(list) ? list : []) {
            if (d.status !== "ready") continue; // выжимать можно только готовые
            const opt = document.createElement("option");
            opt.value = d.id;
            opt.textContent = d.name;
            sel.appendChild(opt);
        }
        if (prev) sel.value = prev; // выбранный документ не сбрасываем
        if (!sumPolling) {
            sumRun.disabled = false;
            const id = +sel.value;
            if (id) await applySummaryState(id);
        }
    } catch (e) {
        // поллинг живёт своей жизнью
    }
}

// applySummaryState — рисует UI строго по фактическому статусу в базе.
async function applySummaryState(id) {
    let s;
    try {
        s = await (await fetch(`/api/summary/${id}`)).json();
    } catch (e) {
        return; // сетевая осечка — повторится на следующем тике
    }
    sumStatus.classList.remove("error");
    switch (s.status) {
        case "running":
            sumStatus.textContent = "Генерирую…";
            sumRun.disabled = true;
            sumPolling = id;
            pollSummary(id);
            break;
        case "ready":
            sumStatus.textContent = "Готово";
            $("#sum-result").textContent = s.text;
            const dl = $("#sum-dl");
            dl.href = `/api/summary/${id}/download`;
            dl.hidden = false;
            sumRun.disabled = false;
            break;
        case "error":
            sumStatus.textContent = "Ошибка: " + (s.error || "неизвестная");
            sumStatus.classList.add("error");
            sumRun.disabled = false;
            break;
        default: // none — выжимку ещё не запускали: чистое состояние
            sumStatus.textContent = "";
            $("#sum-result").textContent = "";
            $("#sum-dl").hidden = true;
            sumRun.disabled = false;
    }
}

sumRun.addEventListener("click", async () => {
    const id = +$("#sum-doc").value;
    if (!id) return;
    sumRun.disabled = true;
    try {
        const r = await fetch("/api/summarize", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ doc_id: id }),
        });
        if (r.status === 409) {
            // Задача уже идёт — не рисуем тупиковую надпись, а подключаемся
            // к её реальному статусу: пользователь увидит «Генерирую…»
            // или готовый результат.
            await applySummaryState(id);
            return;
        }
        if (!r.ok) {
            const j = await r.json().catch(() => ({}));
            sumStatus.textContent = "Не удалось запустить" + (j.error ? ": " + j.error : "");
            sumStatus.classList.add("error");
            sumRun.disabled = false;
            return;
        }
        sumStatus.classList.remove("error");
        sumStatus.textContent = "Генерирую… (это может занять десятки минут)";
        sumPolling = id;
        pollSummary(id);
    } catch (e) {
        sumStatus.textContent = "⚠️ " + e.message;
        sumStatus.classList.add("error");
        sumRun.disabled = false;
    }
});

function pollSummary(id) {
    if (sumPolling === id) return; // уже поллим — второй таймер не нужен
    const timer = setInterval(async () => {
        let s;
        try {
            s = await (await fetch(`/api/summary/${id}`)).json();
        } catch (e) {
            return; // сетевая осечка — попробуем на следующем тике
        }
        if (s.status === "running") return; // ждём дальше
        if (s.status === "none") return;    // статус ещё не появился — тоже ждём
        clearInterval(timer);
        sumPolling = null;
        sumRun.disabled = false;
        applySummaryState(id); // рисуем готовый результат/ошибку через общую точку
    }, 3000);
}

// Смена документа в селекте — тоже синхронизация с реальным статусом:
// у README может быть ready, у книги — none, у третьего — running.
$("#sum-doc").addEventListener("change", () => {
    if (!sumPolling) {
        const id = +$("#sum-doc").value;
        if (id) applySummaryState(id);
    }
});

refreshSumDocs();
