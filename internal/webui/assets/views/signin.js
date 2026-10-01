// Sign-in, as Keera Chat does it: the gateway signs people in, and this
// screen only lists the ways it offers.

import { api } from "../api.js";
import { h, icon, icons, idpMark, replace, brandMark, banner } from "../ui.js";

export async function signIn(root) {
  document.title = "Sign in · Keera Agents";
  // The last sign-in error comes with the config, only once.
  let error = "";
  let cfg = null;
  try {
    cfg = await api.authConfig();
    error = cfg.sign_in_error || "";
  } catch (err) {
    error = err.message;
  }

  const ways = [];
  for (const p of cfg?.providers || []) {
    ways.push(
      h(
        "a",
        { class: "btn btn-signin", href: api.loginURL(p.name) },
        idpMark(p.name) || icon(icons.signout),
        `Continue with ${p.label || p.name}`,
      ),
    );
  }
  if (cfg?.passkeys) {
    ways.push(
      h(
        "a",
        { class: "btn btn-signin", href: api.loginURL("passkey") },
        icon(icons.passkey),
        "Continue with a passkey",
      ),
    );
  }

  root.classList.remove("boot");
  replace(
    root,
    h(
      "div",
      { class: "signin" },
      h(
        "div",
        { class: "signin-card" },
        h("div", { class: "signin-brand" }, brandMark(28), "Keera Agents"),
        h("h1", { class: "signin-title" }, "Welcome"),
        h("p", { class: "signin-lead" }, "Sign in with your Keera account to manage your organisation's agents."),
        error ? banner("bad", { role: "alert" }, error) : null,
        ways.length
          ? h("div", { class: "signin-providers" }, ways)
          : cfg
            ? banner("warn", "This Keera Gateway has no way to sign in set up yet. Ask its administrator.")
            : h("button", { class: "btn", onClick: () => signIn(root) }, "Try again"),
        h(
          "p",
          { class: "signin-note" },
          "Your organisation decides which models agents may use and what they may cost. ",
          cfg?.gateway_url
            ? h("a", { href: cfg.gateway_url, target: "_blank", rel: "noopener" }, "Keera Gateway")
            : "Keera Gateway",
          " keeps the rules.",
        ),
      ),
    ),
  );
}
