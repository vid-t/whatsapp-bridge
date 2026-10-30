// Tests for the activity log wording. Run with `npm test` (node --test).
import assert from "node:assert/strict";
import { describe, test } from "node:test";
import { describeEvent, type ChatName } from "./format.ts";

const FAMILY = "120363000000000001@g.us";
const SAM = "15550100001@s.whatsapp.net";
const ATLAS = "120363000000000002@g.us";
const NAMES: Record<string, string> = { [FAMILY]: "Family", [SAM]: "Sam Lee", [ATLAS]: "Project Atlas" };
const names: ChatName = (jid) => NAMES[jid];

/** The words only, as the Activity list shows them. */
const words = (type: string, detail: string, name?: ChatName) => {
  const e = describeEvent(type, detail, name);
  return { what: e.what, detail: e.detail };
};

/** An mcp.tool line as internal/mcp AuditEvent.Detail writes it. */
const tool = (key: string, name: string, rows: number, rest = "") => `key=${key} tool=${name} args=4be1d07c9a3f5e28 rows=${rows} dur=12ms${rest}`;

describe("send_message", () => {
  test("a send that went through says sent, and names the chat", () => {
    const e = describeEvent("mcp.tool", tool("oa_0d8efbf4838d04f2", "send_message", 1, ` chats=${FAMILY}`), names);
    assert.deepEqual(e, { what: "AI assistant sent a message", detail: "Family · grant oa_0d8efbf4838d04f2", failed: false });
  });

  test("without a chat list the chat shows as its JID", () => {
    assert.deepEqual(words("mcp.tool", tool("5b1f0c2e", "send_message", 1, ` chats=${FAMILY}`)), {
      what: "AI assistant sent a message",
      detail: `${FAMILY} · key 5b1f0c2e`,
    });
  });

  const refused: [code: string, chats: string, detail: string][] = [
    ["duplicate", FAMILY, "refused: same message sent within 2 minutes · Family"],
    ["rate_limited", FAMILY, "refused: rate limit reached · Family"],
    ["not_ticked", FAMILY, "refused: chat not ticked · Family"],
    ["unknown_chat", "", "refused: chat not ticked"],
    ["not_connected", SAM, "refused: account offline · Sam Lee"],
    ["send_not_allowed", "", "refused: not allowed to send"],
    ["unconfirmed", FAMILY, "not confirmed by WhatsApp, may still arrive · Family"],
    ["send_failed", SAM, "failed: WhatsApp refused it · Sam Lee"],
    ["something_new", "", "failed: something_new"],
  ];
  for (const [code, chats, detail] of refused) {
    test(`err=${code} says it did not happen`, () => {
      const e = describeEvent("mcp.tool", tool("oa_0d8efbf4838d04f2", "send_message", 0, (chats ? ` chats=${chats}` : "") + ` err=${code}`), names);
      assert.equal(e.what, "AI assistant tried to send a message");
      assert.equal(e.detail, `${detail} · grant oa_0d8efbf4838d04f2`);
      assert.equal(e.failed, true);
      assert.doesNotMatch(e.what + e.detail, /sent a message/);
    });
  }
});

describe("other tools", () => {
  test("a read names its chats and counts rows", () => {
    assert.deepEqual(describeEvent("mcp.tool", tool("oa_0d8efbf4838d04f2", "search_messages", 6, ` chats=${ATLAS},${FAMILY}`), names), {
      what: "AI assistant searched messages",
      detail: "Project Atlas, Family · 6 rows · grant oa_0d8efbf4838d04f2",
      failed: false,
    });
    assert.deepEqual(words("mcp.tool", tool("oa_1", "read_chat", 1)), { what: "AI assistant read a chat", detail: "1 row · grant oa_1" });
    assert.deepEqual(words("mcp.tool", "key=oa_1 tool=list_chats rows=12"), { what: "AI assistant listed the chats", detail: "12 rows · grant oa_1" });
  });

  test("many chats: three, then how many more", () => {
    const jids = [FAMILY, SAM, ATLAS, "a@g.us", "b@g.us"].join(",");
    assert.equal(describeEvent("mcp.tool", tool("oa_1", "search_messages", 9, ` chats=${jids}`), names).detail, "Family, Sam Lee, Project Atlas and 2 more · 9 rows · grant oa_1");
  });

  test("a failed read says it failed, with no rows", () => {
    assert.deepEqual(describeEvent("mcp.tool", tool("oa_1", "read_chat", 0, " err=unknown_chat")), {
      what: "AI assistant tried to read a chat",
      detail: "refused: chat not ticked · grant oa_1",
      failed: true,
    });
    assert.deepEqual(words("mcp.tool", tool("5b1f0c2e", "get_document", 0, ` chats=${SAM} err=store_error`), names), {
      what: "AI assistant tried to open a document",
      detail: "failed: store_error · Sam Lee · key 5b1f0c2e",
    });
  });

  test("an unknown tool is named, and an inherited name is just a name", () => {
    assert.deepEqual(words("mcp.tool", tool("oa_1", "frobnicate_all", 0, " err=unknown_tool")), {
      what: "AI assistant tried to use frobnicate all",
      detail: "failed: unknown_tool · grant oa_1",
    });
    assert.equal(describeEvent("mcp.tool", tool("oa_1", "constructor", 0)).what, "AI assistant used constructor");
    assert.equal(describeEvent("mcp.tool", tool("oa_1", "toString", 0, " err=toString")).detail, "failed: toString · grant oa_1");
  });
});

describe("message events name the chat", () => {
  test("message.sent", () => {
    const e = describeEvent("message.sent", `chat=${FAMILY} id=3EB0C431D5A7F2B9E104 chars=21 key=oa_0d8efbf4838d04f2`, names);
    assert.deepEqual(e, { what: "Message sent", detail: "Family · 21 characters · grant oa_0d8efbf4838d04f2", failed: false });
  });

  test("message.failed", () => {
    const e = describeEvent("message.failed", `chat=${SAM} key=5b1f0c2e`, names);
    assert.deepEqual(e, { what: "Message not sent", detail: "Sam Lee · key 5b1f0c2e", failed: true });
  });

  test("message.unconfirmed, with or without the message id", () => {
    for (const detail of [`chat=${FAMILY} key=oa_1`, `chat=${FAMILY} id=3EB0C431D5A7F2B9E104 key=oa_1`]) {
      assert.deepEqual(describeEvent("message.unconfirmed", detail, names), {
        what: "Message not confirmed by WhatsApp",
        detail: "Family · grant oa_1",
        failed: true,
      });
    }
  });

  test("a chat the list does not know shows its JID", () => {
    assert.equal(describeEvent("message.sent", "chat=15559999999@s.whatsapp.net id=X chars=3 key=k", names).detail, "15559999999@s.whatsapp.net · 3 characters · key k");
    assert.equal(describeEvent("message.failed", `chat=${SAM} key=k`).detail, `${SAM} · key k`);
  });
});

describe("chat.ticked", () => {
  test("says ticked or unticked, and names the chat", () => {
    assert.deepEqual(words("chat.ticked", `${SAM}=true`, names), { what: "Chat ticked", detail: "Sam Lee" });
    assert.deepEqual(words("chat.ticked", `${SAM}=false`, names), { what: "Chat unticked", detail: "Sam Lee" });
    assert.deepEqual(words("chat.ticked", `${SAM}=false`), { what: "Chat unticked", detail: SAM });
  });
});

// Lines that read the same as before the send fixes (checked against the
// earlier describeEvent).
const UNCHANGED: [type: string, detail: string, what: string, words: string][] = [
  ["state.connected", "", "Connected", ""],
  ["state.disconnected", "stream error", "Disconnected", "stream error"],
  ["pair.success", "device=whatsapp-bridge", "Linked", "as whatsapp-bridge"],
  ["pair.success", "device=My bridge", "Linked", "as My bridge"],
  ["chats.sync", "history sync: 16 chats", "Chats synced", "16 chats"],
  ["chats.unticked", "Taylor Brooks", "Chat unticked", "Taylor Brooks"],
  ["oauth.consent_granted", "client=Claude", "AI assistant connected", "Claude"],
  ["auth.login", "via=code", "Signed in", "with the access code"],
  ["auth.login", "ip=192.0.2.1 via=google email=alex@example.com", "Signed in", "alex@example.com · with google"],
  ["member.approved", "id=m_robin email=robin@example.org by=alex@example.com", "Member approved", "robin@example.org · by alex@example.com"],
  ["history.kept", "3 held messages stored", "History kept", "3 held messages stored"],
  ["key.created", "id=5b1f0c2e kind=mcp", "Key created", ""],
  ["log", "hello", "Note", "hello"],
];

describe("other events read as before", () => {
  for (const [type, detail, what, said] of UNCHANGED) {
    test(`${type} ${JSON.stringify(detail)}`, () => {
      const e = describeEvent(type, detail);
      assert.deepEqual(e, { what, detail: said, failed: false });
      // The chat lookup changes nothing for lines without chats.
      assert.deepEqual(words(type, detail, names), { what, detail: said });
    });
  }
});

describe("consent and keys", () => {
  test("a connection approved with sending says it can send", () => {
    const e = words("oauth.consent_granted", 'client=abc name="Claude" redirect_host=claude.ai by=owner@example.com send=on');
    assert.equal(e.what, "AI assistant connected");
    assert.match(e.detail, / · can send$/);
  });

  test("a connection approved read-only says so", () => {
    assert.match(words("oauth.consent_granted", 'client=abc name="Claude" redirect_host=claude.ai by=owner@example.com send=off').detail, / · read-only$/);
  });

  test("a key created with sending says it can send", () => {
    assert.deepEqual(words("key.created", "id=5b1f0c2e kind=mcp send=on"), { what: "Key created", detail: "can send" });
  });

  test("a key created without sending is read-only", () => {
    assert.deepEqual(words("key.created", "id=5b1f0c2e kind=mcp send=off"), { what: "Key created", detail: "read-only" });
  });
});
