import { test } from "node:test";
import assert from "node:assert/strict";

import {
  amount,
  priceNote,
  quantity,
  hourCost,
} from "../assets/views/billing.js";

test("the price note leaves out what is free", () => {
  assert.equal(
    priceNote({
      cpu_hour_micros: 50000,
      memory_gib_hour_micros: 0,
      disk_gib_month_micros: 120000,
    }),
    "CHF 0.05 a CPU-hour, CHF 0.12 a GiB-month of disk",
  );
  assert.equal(priceNote({}), "");
});

test("amounts have at most two decimals", () => {
  assert.equal(amount(0), "0");
  assert.match(amount(1.23456), /^1[.,]23$/);
});

test("quantities read as internal/runtime reads them", () => {
  assert.equal(quantity("2"), 2);
  assert.equal(quantity("500m"), 0.5);
  assert.equal(quantity(" 4Gi "), 4 * 2 ** 30);
  assert.equal(quantity("1.5G"), 1.5e9);
  assert.ok(Number.isNaN(quantity("lots")));
  assert.ok(Number.isNaN(quantity("Gi")));
  assert.ok(Number.isNaN(quantity("-1")));
});

// The prices and machine of TestHour in internal/billing, so the forms and
// the bill agree.
test("an hour costs what billing.Prices.Hour says", () => {
  const prices = {
    cpu_hour_micros: 1000000,
    memory_gib_hour_micros: 100000,
    disk_gib_month_micros: 7300000,
  };
  const defaults = { cpu: "1", memory: "2Gi", disk: "5Gi" };
  assert.equal(
    hourCost(prices, defaults, { cpu: "500m", memory: "2Gi", disk: "1Gi" }),
    710000,
  );
  // Empty sizes are the defaults: 1.00 + 0.20 + 0.05.
  assert.equal(
    hourCost(prices, defaults, { cpu: "", memory: "", disk: "" }),
    1250000,
  );
  assert.equal(hourCost(prices, defaults, { cpu: "lots" }), null);
});
