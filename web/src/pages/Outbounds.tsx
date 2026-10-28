import { useMemo, useState } from "react";
import { Pencil, Trash2 } from "lucide-react";
import { outboundHooks, routingHooks } from "@/api/policy-hooks";
import type { Outbound } from "@/api/types";
import { Badge, Button, Card, Input, Select, Switch } from "@/components/ui";
import { Modal } from "@/components/Modal";
import { NodePicker } from "@/components/NodePicker";
import { useConfirm } from "@/components/confirm";
import { useToast } from "@/components/toast";
import { useI18n } from "@/i18n/i18n";
import { JsonCodeEditor } from "@/components/JsonCodeEditor";
import { DEFAULT_OUTBOUND_TEMPLATE, parseShareLink } from "@/lib/outbound-uri";
import { Navigate } from "react-router-dom";

const PROTOCOLS = ["freedom", "blackhole", "dns", "vless", "vmess", "trojan", "shadowsocks", "socks", "http", "wireguard"];

// Engines treat an outbound without a tag as unusable and routing rules refer to
// it by name, so the tag is an identifier, not a label.
const TAG_RE = /^[A-Za-z0-9._-]+$/;

type Form = {
  tag: string;
  protocol: string;
  address: string;
  port: string;
  uuid: string;
  password: string;
  method: string;
  security: string;
  sni: string;
  enabled: boolean;
  wgPrivateKey: string;
  wgAddress: string;
  wgEndpoint: string;
  wgPublicKey: string;
  wgReserved: string;
  wgMtu: string;
};

const emptyForm = (): Form => ({
  tag: "",
  protocol: "freedom",
  address: "",
  port: "",
  uuid: "",
  password: "",
  method: "aes-128-gcm",
  security: "none",
  sni: "",
  enabled: true,
  wgPrivateKey: "",
  wgAddress: "",
  wgEndpoint: "",
  wgPublicKey: "",
  wgReserved: "",
  wgMtu: "",
});

// validateForm returns the first thing that would make the outbound unusable.
// The server accepts almost any combination, so the guard has to live here.
function validateForm(f: Form): string | null {
  const tag = f.tag.trim();
  if (!tag) return "Tag is required";
  if (!TAG_RE.test(tag)) return "Tag may only contain letters, digits, dot, dash and underscore";
  if (["freedom", "blackhole", "dns"].includes(f.protocol)) return null;
  if (f.protocol === "wireguard") {
    if (!f.wgPrivateKey.trim()) return "WireGuard private key is required";
    if (!f.wgAddress.trim()) return "WireGuard local address is required";
    return null;
  }
  if (!f.address.trim()) return "Address is required";
  const port = Number(f.port);
  if (!port || port < 1 || port > 65535) return "Port must be between 1 and 65535";
  if ((f.protocol === "vless" || f.protocol === "vmess") && !f.uuid.trim()) return "UUID is required";
  if (["trojan", "shadowsocks", "socks", "http"].includes(f.protocol) && !f.password.trim())
    return "Password is required for this protocol";
  if (f.protocol === "shadowsocks" && !f.method.trim()) return "Encryption method is required";
  if (f.security !== "none" && !f.sni.trim()) return "SNI is required for TLS / REALITY";
  return null;
}

function endpointOf(o: Outbound): string {
  if (o.protocol === "wireguard") {
    const wg = (o.raw?.wireguard ?? {}) as Record<string, unknown>;
    return typeof wg.endpoint === "string" ? wg.endpoint : "";
  }
  if (!o.address) return "";
  return `${o.address}:${o.port}`;
}

/** Tab panel for Routing & Load Balancers */
export function OutboundsTab({ initialNode = "" }: { initialNode?: string }) {
  const { t } = useI18n();
  const [node, setNode] = useState(initialNode);
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<Outbound | null>(null);
  const list = outboundHooks.useList(node || null);
  const rules = routingHooks.useList(node || null);
  const create = outboundHooks.useCreate();
  const update = outboundHooks.useUpdate();
  const del = outboundHooks.useDelete();
  const confirm = useConfirm();
  const toast = useToast();

  const [f, setF] = useState<Form>(emptyForm());
  const set = (k: keyof Form) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setF((s) => ({ ...s, [k]: e.target.value }));
  const isWireguard = f.protocol === "wireguard";
  const proxy = !["freedom", "blackhole", "dns", "wireguard"].includes(f.protocol);

  const [tab, setTab] = useState<"basics" | "json">("basics");
  const [jsonText, setJsonText] = useState("");
  const [jsonErr, setJsonErr] = useState("");
  const [importUri, setImportUri] = useState("");
  const [formErr, setFormErr] = useState("");

  const outbounds = list.data?.outbounds ?? [];

  // How many routing rules point at each tag: deleting a refered outbound leaves
  // those rules matching nothing, so the UI has to say so before it happens.
  const usedBy = useMemo(() => {
    const counts = new Map<string, number>();
    for (const r of rules.data?.rules ?? []) {
      if (r.outbound_tag) counts.set(r.outbound_tag, (counts.get(r.outbound_tag) ?? 0) + 1);
    }
    return counts;
  }, [rules.data]);

  const defaultTemplate = () => JSON.stringify(DEFAULT_OUTBOUND_TEMPLATE, null, 2);

  function gotoJson() {
    setTab("json");
    if (!jsonText) setJsonText(defaultTemplate());
  }

  // importLink converts a pasted share link into outbound JSON in the editor.
  function importLink() {
    setJsonErr("");
    if (!importUri.trim()) return;
    try {
      const obj = parseShareLink(importUri);
      setJsonText(JSON.stringify(obj, null, 2));
      setImportUri("");
    } catch {
      setJsonErr(t("outbounds.importFailed"));
    }
  }

  function shortId() {
    return Math.random().toString(16).slice(2, 6);
  }

  async function submitJSON() {
    setJsonErr("");
    let parsed: Record<string, unknown>;
    try {
      parsed = JSON.parse(jsonText);
    } catch {
      setJsonErr(t("outbounds.invalidJson"));
      return;
    }
    const protocol = String(parsed.protocol ?? "");
    if (!protocol) {
      setJsonErr(t("outbounds.jsonNeedsProtocol"));
      return;
    }
    // 3x-ui templates omit the tag; generate one when absent.
    const tag = String(parsed.tag ?? "") || `out-${protocol}-${shortId()}`;
    parsed.tag = tag;
    if (!TAG_RE.test(tag)) {
      setJsonErr("Tag may only contain letters, digits, dot, dash and underscore");
      return;
    }
    if (outbounds.some((o) => o.tag === tag && o.id !== editing?.id)) {
      setJsonErr(`Tag "${tag}" is already used on this node`);
      return;
    }
    try {
      if (editing) {
        await update.mutateAsync({ id: editing.id, body: { protocol, raw: parsed, enabled: true } });
        toast.success(`${editing.tag} updated`);
        setEditing(null);
      } else {
        await create.mutateAsync({ node_id: node, tag, protocol, raw: parsed, enabled: true });
        toast.success(`${tag} created`);
      }
      closeModal();
    } catch (err) {
      setJsonErr(err instanceof Error ? err.message : t("outbounds.saveFailed"));
    }
  }

  function closeModal() {
    setOpen(false);
    setEditing(null);
    setTab("basics");
    setJsonText("");
    setJsonErr("");
    setImportUri("");
    setFormErr("");
    setF(emptyForm());
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setFormErr("");

    const problem = validateForm(f);
    if (problem) {
      setFormErr(problem);
      return;
    }
    if (outbounds.some((o) => o.tag === f.tag.trim() && o.id !== editing?.id)) {
      setFormErr(`Tag "${f.tag.trim()}" is already used on this node`);
      return;
    }

    const tag = f.tag.trim();
    if (f.protocol === "wireguard") {
      const raw: { wireguard: Record<string, unknown> } = {
        wireguard: {
          private_key: f.wgPrivateKey.trim(),
          address: f.wgAddress.split(",").map((s) => s.trim()).filter(Boolean),
        },
      };
      if (f.wgEndpoint.trim()) raw.wireguard.endpoint = f.wgEndpoint.trim();
      if (f.wgPublicKey.trim()) raw.wireguard.public_key = f.wgPublicKey.trim();
      if (f.wgReserved.trim()) {
        const reserved = f.wgReserved.split(",").map((s) => Number(s.trim())).filter((n) => !Number.isNaN(n));
        if (reserved.length === 3) raw.wireguard.reserved = reserved;
      }
      if (f.wgMtu.trim()) raw.wireguard.mtu = Number(f.wgMtu);
      try {
        if (editing) {
          await update.mutateAsync({ id: editing.id, body: { protocol: "wireguard", raw, enabled: f.enabled } });
          toast.success(`${editing.tag} updated`);
          setEditing(null);
        } else {
          await create.mutateAsync({ node_id: node, tag, protocol: "wireguard", raw, enabled: f.enabled });
          toast.success(`${tag} created`);
        }
        closeModal();
      } catch (err) {
        setFormErr(err instanceof Error ? err.message : t("outbounds.saveFailed"));
      }
      return;
    }

    const body = {
      protocol: f.protocol,
      address: f.address.trim(),
      port: Number(f.port) || 0,
      uuid: f.uuid.trim(),
      password: f.password,
      method: f.method.trim(),
      security: f.security,
      sni: f.sni.trim(),
      enabled: f.enabled,
    };
    try {
      if (editing) {
        await update.mutateAsync({ id: editing.id, body });
        toast.success(`${editing.tag} updated`);
        setEditing(null);
      } else {
        await create.mutateAsync({ node_id: node, tag, ...body });
        toast.success(`${tag} created`);
      }
      closeModal();
    } catch (err) {
      setFormErr(err instanceof Error ? err.message : t("outbounds.saveFailed"));
    }
  }

  function edit(o: Outbound) {
    const wg = (o.raw?.wireguard ?? {}) as Record<string, unknown>;
    const wgAddr = Array.isArray(wg.address) ? (wg.address as unknown[]).map((a) => String(a)).join(", ") : "";
    const wgReserved = Array.isArray(wg.reserved) ? (wg.reserved as unknown[]).map((r) => String(r)).join(",") : "";
    setF({
      tag: o.tag,
      protocol: o.protocol,
      address: o.address,
      port: String(o.port || ""),
      uuid: o.uuid,
      password: o.password,
      method: o.method || "aes-128-gcm",
      security: o.security || "none",
      sni: o.sni,
      enabled: o.enabled !== false,
      wgPrivateKey: typeof wg.private_key === "string" ? wg.private_key : "",
      wgAddress: wgAddr,
      wgEndpoint: typeof wg.endpoint === "string" ? wg.endpoint : "",
      wgPublicKey: typeof wg.public_key === "string" ? wg.public_key : "",
      wgReserved,
      wgMtu: typeof wg.mtu === "number" ? String(wg.mtu) : "",
    });
    setJsonText(o.raw ? JSON.stringify(o.raw, null, 2) : "");
    setJsonErr("");
    setFormErr("");
    setTab("basics");
    setEditing(o);
    setOpen(true);
  }

  async function toggle(o: Outbound) {
    try {
      await update.mutateAsync({
        id: o.id,
        body: {
          protocol: o.protocol,
          address: o.address,
          port: o.port,
          uuid: o.uuid,
          password: o.password,
          username: o.username ?? "",
          method: o.method,
          flow: o.flow ?? "",
          network: o.network ?? "",
          security: o.security,
          sni: o.sni,
          path: o.path ?? "",
          host: o.host ?? "",
          raw: o.raw ?? {},
          enabled: !(o.enabled !== false),
        },
      });
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  }

  async function remove(o: Outbound) {
    const n = usedBy.get(o.tag) ?? 0;
    const ok = await confirm({
      title: `${t("common.delete")} ${o.tag}?`,
      message:
        n > 0
          ? `${n} routing rule${n > 1 ? "s" : ""} point at this outbound and will stop matching.`
          : undefined,
      confirmLabel: t("common.delete"),
      destructive: true,
    });
    if (!ok) return;
    await del.mutateAsync(o.id);
    toast.success(t("common.delete"));
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-end gap-2">
        <NodePicker value={node} onChange={setNode} />
        <Button
          onClick={() => {
            setF(emptyForm());
            setFormErr("");
            setOpen(true);
          }}
          disabled={!node}
        >
          {t("common.add")}
        </Button>
      </div>

      {!node ? (
        <Card className="p-8 text-center text-sm text-fg-muted">
          {t("x.pickNodeOutbounds")}
        </Card>
      ) : (
        <Card className="p-0">
          <div className="divide-y divide-white/[0.05]">
            {outbounds.map((o) => {
              const endpoint = endpointOf(o);
              const n = usedBy.get(o.tag) ?? 0;
              const enabled = o.enabled !== false;
              return (
                <div
                  key={o.id}
                  className={`flex items-center justify-between gap-3 px-5 py-3 ${enabled ? "" : "opacity-55"}`}
                >
                  <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm">
                    <span className="font-medium">{o.tag}</span>
                    <Badge>{o.protocol}</Badge>
                    {endpoint && <span className="font-mono text-xs text-fg-muted">{endpoint}</span>}
                    {o.security && o.security !== "none" && <Badge color="on_hold">{o.security}</Badge>}
                    {o.sni && <span className="text-xs text-fg-subtle">sni {o.sni}</span>}
                    {n > 0 && <span className="text-xs text-fg-subtle">used by {n} rule{n > 1 ? "s" : ""}</span>}
                    {!enabled && <Badge color="warning">disabled</Badge>}
                  </div>
                  <div className="flex flex-shrink-0 items-center gap-1">
                    <Switch checked={enabled} onCheckedChange={() => void toggle(o)} />
                    <Button variant="ghost" size="sm" onClick={() => edit(o)} title="Edit">
                      <Pencil size={15} />
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      className="text-danger"
                      onClick={() => void remove(o)}
                      title="Delete"
                    >
                      <Trash2 size={15} />
                    </Button>
                  </div>
                </div>
              );
            })}
            {outbounds.length === 0 && (
              <p className="px-5 py-8 text-center text-sm text-fg-muted">{t("common.none")}</p>
            )}
          </div>
        </Card>
      )}

      <Modal open={open} onClose={closeModal} title={editing ? `Edit ${editing.tag}` : t("nav.outbounds")} size="lg">
        {/* Tabs */}
        <div className="mb-4 flex gap-5 border-b border-border/60 text-sm">
          {(["basics", "json"] as const).map((tk) => (
            <button
              key={tk}
              type="button"
              onClick={() => (tk === "json" ? gotoJson() : setTab("basics"))}
              className={`-mb-px border-b-2 pb-2 font-medium transition ${tab === tk ? "border-primary text-primary" : "border-transparent text-fg-muted hover:text-fg"}`}
            >
              {tk === "basics" ? t("outbounds.basics") : "JSON"}
            </button>
          ))}
        </div>

        {tab === "basics" ? (
          <form onSubmit={submit} className="space-y-3">
            <div className="grid grid-cols-2 gap-2">
              <Input
                placeholder="Tag (used by routing rules)"
                value={f.tag}
                onChange={set("tag")}
                required
                disabled={!!editing}
              />
              <Select value={f.protocol} onChange={set("protocol")}>
                {PROTOCOLS.map((p) => (
                  <option key={p} value={p}>
                    {p}
                  </option>
                ))}
              </Select>
            </div>
            {editing && (
              <p className="text-xs text-fg-subtle">
                Tag is the routing target name; it cannot be renamed here.
              </p>
            )}

            {isWireguard && (
              <>
                <Input
                  placeholder="WireGuard private key"
                  value={f.wgPrivateKey}
                  onChange={set("wgPrivateKey")}
                  required
                  dir="ltr"
                  className="font-mono text-xs"
                />
                <Input
                  placeholder="Local addresses (comma-separated, e.g. 172.16.0.2/32, fd01::5/128)"
                  value={f.wgAddress}
                  onChange={set("wgAddress")}
                  required
                  dir="ltr"
                  className="font-mono text-xs"
                />
                <Input
                  placeholder="Endpoint (optional, default engage.cloudflareclient.com:2408)"
                  value={f.wgEndpoint}
                  onChange={set("wgEndpoint")}
                  dir="ltr"
                  className="font-mono text-xs"
                />
                <Input
                  placeholder="Peer public key (optional, default Cloudflare WARP)"
                  value={f.wgPublicKey}
                  onChange={set("wgPublicKey")}
                  dir="ltr"
                  className="font-mono text-xs"
                />
                <div className="grid grid-cols-2 gap-2">
                  <Input
                    placeholder="Reserved (optional, 3 comma-separated ints, e.g. 171,48,225)"
                    value={f.wgReserved}
                    onChange={set("wgReserved")}
                    dir="ltr"
                    className="font-mono text-xs"
                  />
                  <Input
                    placeholder="MTU (optional, default 1280)"
                    value={f.wgMtu}
                    onChange={set("wgMtu")}
                    inputMode="numeric"
                  />
                </div>
                <div className="flex items-center gap-3">
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    onClick={() =>
                      setF((s) => ({
                        ...s,
                        wgEndpoint: "engage.cloudflareclient.com:2408",
                        wgPublicKey: "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=",
                      }))
                    }
                  >
                    WARP defaults
                  </Button>
                  <span className="text-xs text-fg-muted">
                    WARP needs a registered key — paste private_key/address/reserved from warp-cli.
                  </span>
                </div>
              </>
            )}

            {proxy && (
              <>
                <div className="grid grid-cols-2 gap-2">
                  <Input placeholder="Address" value={f.address} onChange={set("address")} />
                  <Input placeholder="Port" value={f.port} onChange={set("port")} inputMode="numeric" />
                </div>
                {(f.protocol === "vless" || f.protocol === "vmess") && (
                  <Input placeholder="UUID" value={f.uuid} onChange={set("uuid")} />
                )}
                {["trojan", "shadowsocks", "socks", "http"].includes(f.protocol) && (
                  <Input placeholder="Password" value={f.password} onChange={set("password")} />
                )}
                {f.protocol === "shadowsocks" && (
                  <Input placeholder="Method" value={f.method} onChange={set("method")} />
                )}
                <div className="grid grid-cols-2 gap-2">
                  <Select value={f.security} onChange={set("security")}>
                    <option value="none">none</option>
                    <option value="tls">tls</option>
                    <option value="reality">reality</option>
                  </Select>
                  <Input
                    placeholder={f.security === "none" ? "SNI (unused)" : "SNI (required)"}
                    value={f.sni}
                    onChange={set("sni")}
                    disabled={f.security === "none"}
                  />
                </div>
              </>
            )}

            <div className="flex items-center justify-between">
              <Switch
                checked={f.enabled}
                onCheckedChange={(v) => setF((s) => ({ ...s, enabled: v }))}
                label={t("x.enabled")}
              />
              {formErr && <span className="text-xs text-danger">{formErr}</span>}
            </div>

            <div className="flex justify-end gap-2 pt-1">
              <Button type="button" variant="ghost" onClick={closeModal}>
                {t("common.cancel")}
              </Button>
              <Button type="submit" disabled={create.isPending || update.isPending}>
                {editing ? t("common.save") : t("common.create")}
              </Button>
            </div>
          </form>
        ) : (
          <div className="space-y-3">
            {/* Share-link import bar */}
            <div className="flex gap-2">
              <Input
                value={importUri}
                onChange={(e) => setImportUri(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    importLink();
                  }
                }}
                placeholder="vmess:// vless:// trojan:// ss:// hysteria2:// wireguard://"
                className="flex-1 font-mono text-xs"
                dir="ltr"
              />
              <Button type="button" onClick={importLink}>
                {t("outbounds.import")}
              </Button>
            </div>

            <JsonCodeEditor value={jsonText} onChange={setJsonText} rows={16} />

            {jsonErr && <p className="text-sm text-danger">{jsonErr}</p>}
            <div className="flex justify-end gap-2 pt-1">
              <Button type="button" variant="ghost" onClick={closeModal}>
                {t("common.cancel")}
              </Button>
              <Button type="button" onClick={submitJSON} disabled={create.isPending || update.isPending}>
                {editing ? t("common.save") : t("common.create")}
              </Button>
            </div>
          </div>
        )}
      </Modal>
    </div>
  );
}

/** @deprecated — use /routing?tab=outbounds */
export function Outbounds() {
  return <Navigate to="/routing?tab=outbounds" replace />;
}
