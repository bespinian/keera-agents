// The dashboard's chart, drawn as inline SVG by hand, as Keera Gateway's: a
// charting library would be the largest dependency in a binary with almost
// none.

import { h, svg, compact, num, empty, plural } from "./ui.js";

const PAD = { top: 12, right: 8, bottom: 22, left: 42 };

// The width before the chart can be measured, close to a real card so the
// first paint does not shift.
const GUESS_W = 900;
const MIN_W = 280;

/**
 * areaChart draws tasks over time with failures as a baseline bar.
 * points: [{ at, tasks, failed }]
 */
export function areaChart(points, { height = 190, bucket = "day" } = {}) {
  if (!points || points.every((p) => !p.tasks)) {
    return h("div", { class: "chart-holder" }, empty("No tasks in this period"));
  }
  // A single point has no line, so it becomes a flat pair.
  const data = points.length === 1 ? [points[0], { ...points[0] }] : points;
  return responsive((w, tip) => paintArea(data, w, height, { bucket, tip }));
}

// responsive draws in real pixels, as a stretched grid would stretch the
// labels too. A detached element has no width, so it is drawn at a guess and
// redrawn once its width is known.
function responsive(paint) {
  const holder = h("div", { class: "chart-holder" });
  const tip = h("div", { class: "chart-tip", style: { display: "none" } });
  let drawnAt = 0;
  function draw(width) {
    const w = Math.max(MIN_W, Math.round(width));
    if (w === drawnAt) return;
    drawnAt = w;
    const plot = paint(w, tip);
    const old = holder.querySelector("svg.chart");
    if (old) {
      // The shape the pointer was over is gone.
      tip.style.display = "none";
      old.replaceWith(plot);
    } else {
      holder.prepend(plot);
    }
  }

  draw(GUESS_W);
  holder.append(tip);
  // The height does not follow the width, so a redraw cannot cause another
  // resize.
  new ResizeObserver((entries) => {
    draw(entries[entries.length - 1].contentRect.width);
  }).observe(holder);
  return holder;
}

function paintArea(data, W, H, { bucket, tip }) {
  const n = data.length;
  const innerW = W - PAD.left - PAD.right;
  const innerH = H - PAD.top - PAD.bottom;
  const niceMax = niceCeil(Math.max(1, ...data.map((p) => p.tasks)));
  const x = (i) => PAD.left + (n === 1 ? innerW / 2 : (i / (n - 1)) * innerW);
  const y = (v) => PAD.top + innerH - (v / niceMax) * innerH;

  // Counts of tasks are whole, so a scale up to 1 has no half line.
  const ticks = [...new Set([0, 0.5, 1].map((f) => Math.round(niceMax * f)))];
  const grid = ticks.map((t) => [
    svg("line", {
      class: "grid-line",
      x1: PAD.left,
      x2: W - PAD.right,
      y1: y(t).toFixed(1),
      y2: y(t).toFixed(1),
    }),
    svg("text", { class: "axis-text", x: PAD.left - 7, y: (y(t) + 3.5).toFixed(1), "text-anchor": "end" }, compact(t)),
  ]);

  const line = data.map((p, i) => `${i === 0 ? "M" : "L"}${x(i).toFixed(1)},${y(p.tasks).toFixed(1)}`).join(" ");
  const base = (PAD.top + innerH).toFixed(1);
  const area = `${line} L${x(n - 1).toFixed(1)},${base} L${x(0).toFixed(1)},${base} Z`;

  // Failures are short bars along the baseline: they are rare enough to
  // vanish on the tasks' scale, and matter more.
  const maxErr = Math.max(1, ...data.map((p) => p.failed));
  const barW = Math.max(1.5, Math.min(9, innerW / n - 1.5));
  const errBars = data.map((p, i) => {
    if (!(p.failed > 0)) return null;
    const bh = (p.failed / maxErr) * (innerH * 0.28);
    return svg("rect", {
      class: "err-bar",
      x: (x(i) - barW / 2).toFixed(1),
      y: (PAD.top + innerH - bh).toFixed(1),
      width: barW.toFixed(1),
      height: bh.toFixed(1),
      rx: 1,
    });
  });

  const cursor = svg("line", {
    class: "cursor-line",
    x1: 0,
    x2: 0,
    y1: PAD.top,
    y2: PAD.top + innerH,
    opacity: 0,
  });

  const hits = data.map((p, i) => {
    const w = innerW / n;
    return svg("rect", {
      class: "hit",
      x: (PAD.left + i * w).toFixed(1),
      y: PAD.top,
      width: w.toFixed(1),
      height: innerH,
      onMouseenter: () => {
        cursor.setAttribute("x1", x(i).toFixed(1));
        cursor.setAttribute("x2", x(i).toFixed(1));
        cursor.setAttribute("opacity", 1);
        tip.style.display = "";
        tip.replaceChildren(
          h("div", { class: "t" }, bucketLabel(p.at, bucket, true)),
          h("div", { class: "muted" }, `${num(p.tasks)} ${plural(p.tasks, "task")}`),
          p.failed ? h("div", { class: "muted" }, `${num(p.failed)} failed`) : null,
        );
        // The tip sits right of the cursor, or left of it where it would not
        // fit, measured as its width follows the labels.
        const tw = tip.offsetWidth;
        let left = x(i) + 12;
        if (left + tw > W) left = Math.max(0, x(i) - 12 - tw);
        tip.style.left = `${left.toFixed(1)}px`;
        tip.style.top = "6px";
      },
      onMouseleave: () => {
        cursor.setAttribute("opacity", 0);
        tip.style.display = "none";
      },
    });
  });

  // Label as many positions as fit: a date needs about 70 pixels. The last
  // bucket is always named, and a label that would overlap it is dropped.
  const last = n - 1;
  const step = Math.max(1, Math.ceil(n / Math.max(2, Math.floor(innerW / 70))));
  const labelled = new Set([last]);
  for (let i = 0; i < last; i += step) {
    if (x(last) - x(i) >= 60) labelled.add(i);
  }
  const labels = data.map((p, i) =>
    labelled.has(i)
      ? svg(
          "text",
          {
            class: "axis-text",
            x: x(i).toFixed(1),
            y: H - 6,
            "text-anchor": i === 0 ? "start" : i === last ? "end" : "middle",
          },
          bucketLabel(p.at, bucket, false),
        )
      : null,
  );

  return svg(
    "svg",
    { class: "chart", viewBox: `0 0 ${W} ${H}`, width: W, height: H, role: "img", "aria-label": "Tasks over time" },
    grid,
    svg("path", { class: "area", d: area }),
    svg("path", { class: "line", d: line }),
    errBars,
    cursor,
    labels,
    hits,
  );
}

function bucketLabel(iso, bucket, long) {
  const d = new Date(iso);
  if (bucket === "hour") {
    return long
      ? d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })
      : d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  }
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

/** niceCeil rounds a scale's top up to 1, 1.5, 2, 3 or 5 times a power of ten. */
function niceCeil(v) {
  const mag = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 1.5, 2, 3, 5, 10]) {
    if (v <= m * mag) return m * mag;
  }
  return 10 * mag;
}
