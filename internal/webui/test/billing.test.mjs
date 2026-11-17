import { test } from "node:test";
import assert from "node:assert/strict";

import { amount, priceNote } from "../assets/views/billing.js";

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
