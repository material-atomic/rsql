/**
 * The order → payment → ledger example, running.
 *
 * This is the flow every database gets asked for and normally answers with an
 * interactive transaction: begin, read the order, check it is still unpaid,
 * insert the payment, post two ledger lines, commit — with the row locked and
 * everybody else waiting on whatever the client does in between.
 *
 * Here there is no begin. `orders.pay` is one declared operation made of four
 * steps, and the operation is the transaction. Its cost is known before it
 * runs, the client cannot hold it open, and the conditions it checks are
 * written down in schema.json where they can be read by someone deciding
 * whether to trust it.
 *
 * Run it:
 *
 *   RSQL_SERVER_BIN=…/rsqld RSQL_CLI_BIN=…/rsql node examples/ledger/run.mjs
 *
 * RSQL_CLIENT may point at the built driver; it defaults to the sibling
 * ecosy-rsql package in this workspace.
 */

import { spawn, execFileSync } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { createServer } from "node:net";

const here = dirname(fileURLToPath(import.meta.url));
const SERVER = process.env.RSQL_SERVER_BIN;
const CLI = process.env.RSQL_CLI_BIN;
const CLIENT = process.env.RSQL_CLIENT ?? resolve(here, "../../../ecosy-rsql/dist");

if (!SERVER || !CLI) {
  console.error("set RSQL_SERVER_BIN and RSQL_CLI_BIN to the built binaries");
  process.exit(2);
}

const { Client } = await import(pathToFileURL(join(CLIENT, "client/index.mjs")));
const { nodeTransport } = await import(pathToFileURL(join(CLIENT, "node/index.mjs")));
const { sign } = await import(pathToFileURL(join(CLIENT, "signer/index.mjs")));

const SECRET = "an-example-secret";
const PASSWORD = "an-example-password";

const say = (line) => console.log(line);
const money = (n) => (n < 0 ? `-${(-n).toFixed(2)}` : ` ${n.toFixed(2)}`);

async function freePort() {
  return new Promise((ok, no) => {
    const probe = createServer();
    probe.once("error", no);
    probe.listen(0, "127.0.0.1", () => {
      const { port } = probe.address();
      probe.close(() => ok(port));
    });
  });
}

const dir = mkdtempSync(join(tmpdir(), "rsql-ledger-"));
const env = { ...process.env, RSQL_SECRET: SECRET, RSQL_DIR: dir, RSQL_ACCOUNT: "acme", RSQL_DB: "books" };

say(`a database in ${dir}`);
execFileSync(CLI, ["apply", join(here, "schema.json")], { env, stdio: "inherit" });
say("declared: three collections, five operations, one of them a batch of four steps\n");

const port = await freePort();
const server = spawn(SERVER, [], {
  env: { ...env, RSQL_INSECURE: "1", RSQL_ADDR: `127.0.0.1:${port}` },
  stdio: ["ignore", "pipe", "inherit"],
});
await new Promise((ok, no) => {
  const timer = setTimeout(() => no(new Error("the server did not announce itself")), 5000);
  server.stdout.on("data", (chunk) => String(chunk).includes("listening") && (clearTimeout(timer), ok()));
  server.once("error", no);
});

const sig = await sign({ accountId: "acme", password: PASSWORD, dbname: "books" }, { secret: SECRET });
const url = `rsql://acme:${PASSWORD}@127.0.0.1:${port}/books?sig=${sig}`;
const client = new (Client({ transport: nodeTransport({ insecure: true }), mode: "bound", requestTimeout: 5000 }))();

let failed = false;
const must = (what, ok) => {
  say(`${ok ? "  ok  " : "  NO  "} ${what}`);
  if (!ok) failed = true;
};

try {
  const at = Date.now();

  say("an order is placed");
  await client.invoke(url, "orders.place", { id: "ord-1001", customer: "cus-7", total: 249.5 }, { write: true });
  const placed = await client.invoke(url, "orders.get", { id: "ord-1001" });
  say(`  ord-1001  ${placed.rows[0].status}  ${money(placed.rows[0].total)}\n`);

  say("it is paid — one call, four writes, one transaction");
  const paid = await client.invoke(
    url,
    "orders.pay",
    { order: "ord-1001", amount: 249.5, at, reference: "stripe_pi_3Qx" },
    { write: true },
  );
  say(`  ${paid.changed} documents changed\n`);

  const after = await client.invoke(url, "orders.get", { id: "ord-1001" });
  const payments = await client.invoke(url, "payments.of_order", { order: "ord-1001" });
  const cash = await client.invoke(url, "entries.of_account", { account: "assets:cash" });
  const sales = await client.invoke(url, "entries.of_account", { account: "income:sales" });

  say("what the books now say");
  say(`  orders      ord-1001            ${after.rows[0].status}`);
  say(`  payments    ${payments.rows[0].reference}      ${money(payments.rows[0].amount)}  → ${payments.rows[0].order}`);
  say(`  entries     assets:cash  debit  ${money(cash.rows[0].amount)}`);
  say(`  entries     income:sales credit ${money(sales.rows[0].amount)}`);
  const balance = cash.rows.reduce((t, e) => t + e.amount, 0) - sales.rows.reduce((t, e) => t + e.amount, 0);
  say(`  balance                        ${money(balance)}\n`);

  must("the order is paid", after.rows[0].status === "paid");
  must("the payment points at the order", payments.rows[0].order === "ord-1001");
  must("one debit and one credit", cash.rows.length === 1 && sales.rows.length === 1);
  must("the ledger balances", balance === 0);

  say("\nthe same payment arrives again — a retry, a double-click, a webhook replayed");
  let refused;
  try {
    await client.invoke(
      url,
      "orders.pay",
      { order: "ord-1001", amount: 249.5, at, reference: "stripe_pi_3Qx" },
      { write: true },
    );
  } catch (error) {
    refused = error;
    say(`  refused: ${error.code} — ${error.message}`);
  }
  // The code, not the sentence. A driver deciding whether to retry cannot read
  // prose, and "condition" is the one answer that means: the world moved, read
  // it again — retrying this exact call will fail the same way forever.
  must("the second payment was refused", refused?.code === "condition");

  const stillOne = await client.invoke(url, "payments.of_order", { order: "ord-1001" });
  const stillCash = await client.invoke(url, "entries.of_account", { account: "assets:cash" });
  must("no second payment was written", stillOne.rows.length === 1);
  must("no ledger line was left behind by the refused call", stillCash.rows.length === 1);

  say("\nand an order paid the wrong amount");
  await client.invoke(url, "orders.place", { id: "ord-1002", customer: "cus-7", total: 40 }, { write: true });
  let wrong;
  try {
    await client.invoke(url, "orders.pay", { order: "ord-1002", amount: 39.99, at, reference: "short" }, { write: true });
  } catch (error) {
    wrong = error;
    say(`  refused: ${error.code} — ${error.message}`);
  }
  must("a payment that does not match the total was refused", wrong?.code === "condition");
  const short = await client.invoke(url, "orders.get", { id: "ord-1002" });
  must("the order it half-wrote is untouched", short.rows[0].status === "awaiting_payment");
  must("the ledger still balances", (await client.invoke(url, "entries.of_account", { account: "assets:cash" })).rows.length === 1);
} finally {
  await client.close();
  server.kill("SIGTERM");
}

say(failed ? "\nsomething is wrong" : "\nall of it held");
process.exit(failed ? 1 : 0);
