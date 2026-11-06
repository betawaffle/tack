// Money for the page, as whole picodollars. Everything here works on digits rather than
// float arithmetic, so what's shown and what's typed are exact.
"use strict";

const tackMoney = (() => {
  const CENT_DIGITS = 10;
  const DOLLAR_DIGITS = 12;
  const digitsFor = (unit) => (unit === "dollars" ? DOLLAR_DIGITS : CENT_DIGITS);

  // The number as a decimal, moved along by that many digits.
  function decimal(pico, digits) {
    const text = String(pico).padStart(digits + 1, "0");
    const fraction = text.slice(-digits).replace(/0+$/, "");
    return text.slice(0, -digits) + (fraction ? "." + fraction : "");
  }

  // Under a dollar it's cents, with all the precision there is. From a dollar up, it's
  // dollars and cents, and a + says there's more than that rather than rounding.
  function format(pico) {
    if (pico < 1e12) return decimal(pico, CENT_DIGITS) + "¢";
    const text = String(pico);
    const rest = text.slice(-CENT_DIGITS);
    const cents = text.slice(0, -CENT_DIGITS).padStart(3, "0");
    return `$${cents.slice(0, -2)}.${cents.slice(-2)}${/[1-9]/.test(rest) ? "+" : ""}`;
  }

  // For guesses, where a long number would claim precision that isn't there.
  function rough(pico) {
    if (pico <= 0) return format(0);
    const unit = 10 ** Math.max(String(Math.round(pico)).length - 2, 0);
    return format(Math.round(pico / unit) * unit);
  }

  // What's typed, as picodollars, or null if it isn't an amount that can be represented.
  function parse(text, unit) {
    const match = /^(\d*)(?:\.(\d*))?$/.exec(text.trim());
    if (!match || (!match[1] && !match[2])) return null;
    const digits = digitsFor(unit);
    const fraction = match[2] ?? "";
    if (fraction.replace(/0+$/, "").length > digits) return null;
    const pico = Number((match[1] || "0") + fraction.padEnd(digits, "0").slice(0, digits));
    return Number.isSafeInteger(pico) ? pico : null;
  }

  // The exact amount for a box, without a symbol.
  function toText(pico, unit) {
    return decimal(pico, digitsFor(unit));
  }

  return { format, rough, parse, toText };
})();

if (typeof module !== "undefined") module.exports = tackMoney;
