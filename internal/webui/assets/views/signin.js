// Sign-in, as Keera Chat does it: the gateway signs people in, and this
// screen only lists the ways it offers. As on the gateway, the operator key
// is asked for when there is no other way in, or at ?operator_key=1.

import { api } from "../api.js";
import { h, icon, icons, idpMark, replace, brandMark, banner, field } from "../ui.js";

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
  // A browser signed in to the gateway's panel comes straight back signed
  // in. Otherwise it comes back here, and the buttons show.
  const asked = new URLSearchParams(location.search).get("operator_key") === "1";
  if (cfg?.silent && !asked) {
    location.replace(api.silentLoginURL());
    return;
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

  // Once single sign-on works, the key is only offered at ?operator_key=1.
  // A passkey account is never an operator, so passkeys alone keep it.
  const sso = (cfg?.providers || []).length > 0;
  const keyForm = cfg && (!sso || asked) ? operatorKeyForm() : null;

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
            ? null
            : h("button", { class: "btn", onClick: () => signIn(root) }, "Try again"),
        keyForm && ways.length ? h("div", { class: "divider" }, "operator") : null,
        keyForm,
        h(
          "p",
          { class: "signin-note" },
          keyForm && !ways.length
            ? "Keera Gateway has no identity provider set up, so only its operator key works. "
            : "Your organisation decides which models agents may use and what they may cost. ",
          cfg?.gateway_url
            ? h("a", { href: cfg.gateway_url, target: "_blank", rel: "noopener" }, "Keera Gateway")
            : "Keera Gateway",
          " keeps the rules.",
        ),
      ),
    ),
  );
  if (keyForm && (asked || !ways.length)) keyForm.querySelector("input").focus();
}

// operatorKeyForm signs in with the gateway's operator key, which the gateway
// checks. It signs in as an operator, who sees every organisation.
function operatorKeyForm() {
  const message = h("div");
  const input = h("input", {
    class: "input",
    type: "password",
    id: "operator-key",
    placeholder: "keera operator key",
    autocomplete: "current-password",
  });
  const label = "Sign in with the operator key";
  const button = h("button", { class: "btn btn-primary", type: "submit" }, label);
  return h(
    "form",
    {
      onSubmit: async (e) => {
        e.preventDefault();
        replace(message);
        const key = input.value.trim();
        if (!key) {
          replace(message, banner("bad", "Enter the operator key."));
          input.focus();
          return;
        }
        button.disabled = true;
        button.textContent = "Signing in…";
        try {
          await api.operatorSignIn(key);
          location.replace("/");
        } catch (err) {
          replace(message, banner("bad", err.message || "Sign-in failed."));
          button.disabled = false;
          button.textContent = label;
          input.focus();
          input.select();
        }
      },
    },
    message,
    field("Operator key", input, "Keera Gateway's KEERA_OPERATOR_KEY. It signs you in as an operator."),
    button,
  );
}
