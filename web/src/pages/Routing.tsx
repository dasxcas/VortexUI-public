import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { ArrowDown, ArrowUp, Pencil, Plus, Trash2, Waypoints } from "lucide-react";
import { useNodeInbounds } from "@/api/hooks";
import { balancerHooks, outboundHooks, routingHooks } from "@/api/policy-hooks";
import type { RoutingRule } from "@/api/types";
import { Badge, Button, Input, Select, Switch } from "@/components/ui";
import { GlassCard } from "@/components/vortexui";
import { Modal } from "@/components/Modal";
import { NodePicker } from "@/components/NodePicker";
import { useConfirm } from "@/components/confirm";
import { useToast } from "@/components/toast";
import { useI18n } from "@/i18n/i18n";
import { useTitle } from "@/lib/useTitle";

const csv = (s: string) => (s ? s.split(",").map((x) => x.trim()).filter(Boolean) : []);

const PROTOCOLS = ["http", "tls", "quic", "bittorrent"];
const NETWORKS = [
  { value: "", label: "any network" },
  { value: "tcp", label: "tcp" },
  { value: "udp", label: "udp" },
  { value: "tcp,udp", label: "tcp + udp" },
];

type Form = {
  name: string;
  priority: string;
  inbound_tags: string[];
  domains: string;
  ip: string;
  port: string;
  protocols: string[];
  network: string;
  target: string;
  enabled: boolean;
};

const emptyForm = (priority: number): Form => ({
  name: "",
  priority: String(priority),
  inbound_tags: [],
  domains: "",
  ip: "",
  port: "",
  protocols: [],
  network: "",
  target: "",
  enabled: true,
});

// A rule that {t("x.matchesNothing")} never fires. Count matchers so the form can say so
// before saving instead of leaving a dead rule in the list.
function matcherCount(f: Form) {
  return (
    f.inbound_tags.length +
    csv(f.domains).length +
    csv(f.ip).length +
    f.protocols.length +
    (f.port.trim() ? 1 : 0) +
    (f.network ? 1 : 0)
  );
}

// One-line summary of what a rule matches, so the list is readable without
// opening every rule.
function matcherSummary(r: RoutingRule): string[] {
  const out: string[] = [];
  if (r.inbound_tags?.length) out.push(`in ${r.inbound_tags.join(", ")}`);
  if (r.domains?.length) out.push(`${r.domains.length} domain${r.domains.length > 1 ? "s" : ""}`);
  if (r.ip?.length) out.push(`${r.ip.length} ip`);
  if (r.port) out.push(`port ${r.port}`);
  if (r.protocols?.length) out.push(r.protocols.join("/"));
  if (r.network) out.push(r.network);
  return out;
}

function bodyOf(r: RoutingRule, priority: number) {
  return {
    node_id: r.node_id,
    priority,
    name: r.name,
    inbound_tags: r.inbound_tags ?? [],
    domains: r.domains ?? [],
    ip: r.ip ?? [],
    port: r.port,
    protocols: r.protocols ?? [],
    network: r.network,
    outbound_tag: r.outbound_tag,
    balancer_tag: r.balancer_tag,
    enabled: r.enabled,
  };
}

export function Routing() {
  useTitle("Routing");
  const { t } = useI18n();
  const [params, setParams] = useSearchParams();
  const [node, setNodeState] = useState(params.get("node") ?? "");
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<RoutingRule | null>(null);
  const [f, setF] = useState<Form>(emptyForm(1));

  const list = routingHooks.useList(node || null);
  const create = routingHooks.useCreate();
  const update = routingHooks.useUpdate();
  const del = routingHooks.useDelete();
  const inbounds = useNodeInbounds(node || null);
  const outbounds = outboundHooks.useList(node || null);
  const balancers = balancerHooks.useList(node || null);

  const confirm = useConfirm();
  const toast = useToast();

  // Keep the picker and the URL in step, so a node's rules can be linked to
  // (the node menu sends you here with ?node=<id>).
  function setNode(id: string) {
    setNodeState(id);
    const next = new URLSearchParams(params);
    if (id) next.set("node", id);
    else next.delete("node");
    setParams(next, { replace: true });
  }

  useEffect(() => {
    const fromURL = params.get("node") ?? "";
    if (fromURL !== node) setNodeState(fromURL);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [params]);

  const sorted = useMemo(
    // The API returns {"rules": [...]}; reading "routing" here silently showed
    // an empty list no matter how many rules existed.
    () => [...(list.data?.rules ?? [])].sort((a, b) => a.priority - b.priority),
    [list.data],
  );

  const inboundTags = (inbounds.data?.inbounds ?? []).map((i) => i.tag).filter(Boolean);
  const targetOptions = useMemo(() => {
    const outs = (outbounds.data?.outbounds ?? []).map((o) => `out:${o.tag}`);
    const bals = (balancers.data?.balancers ?? []).map((b) => `bal:${b.tag}`);
    return [...outs, ...bals];
  }, [outbounds.data, balancers.data]);

  const set = (k: keyof Form) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setF((s) => ({ ...s, [k]: e.target.value }));

  function toggleList(key: "inbound_tags" | "protocols", value: string) {
    setF((s) => {
      const cur = s[key];
      return { ...s, [key]: cur.includes(value) ? cur.filter((v) => v !== value) : [...cur, value] };
    });
  }

  function openNew() {
    const nextPriority = sorted.length ? Math.max(...sorted.map((r) => r.priority)) + 1 : 1;
    setEditing(null);
    setF(emptyForm(nextPriority));
    setOpen(true);
  }

  function edit(r: RoutingRule) {
    const target = r.balancer_tag ? `bal:${r.balancer_tag}` : `out:${r.outbound_tag}`;
    setF({
      name: r.name,
      priority: String(r.priority),
      inbound_tags: r.inbound_tags ?? [],
      domains: (r.domains ?? []).join(", "),
      ip: (r.ip ?? []).join(", "),
      port: r.port,
      protocols: r.protocols ?? [],
      network: r.network,
      target,
      enabled: r.enabled,
    });
    setEditing(r);
    setOpen(true);
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!node) {
      toast.error("Pick a node first");
      return;
    }
    if (matcherCount(f) === 0) {
      toast.error("A rule needs at least one matcher");
      return;
    }
    if (!f.target) {
      toast.error("Pick a target");
      return;
    }
    const [kind, tag] = f.target.split(":");
    const body = {
      node_id: node,
      name: f.name,
      priority: Number(f.priority) || 1,
      inbound_tags: f.inbound_tags,
      domains: csv(f.domains),
      ip: csv(f.ip),
      port: f.port.trim(),
      protocols: f.protocols,
      network: f.network,
      outbound_tag: kind === "out" ? tag : "",
      balancer_tag: kind === "bal" ? tag : "",
      enabled: f.enabled,
    };
    try {
      if (editing) {
        await update.mutateAsync({ id: editing.id, body });
        toast.success("Rule updated");
      } else {
        await create.mutateAsync(body);
        toast.success("Rule created");
      }
      setOpen(false);
      setEditing(null);
      setF(emptyForm(1));
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  }

  async function remove(r: RoutingRule) {
    if (
      await confirm({
        title: `Delete ${r.name || "rule"}?`,
        confirmLabel: t("common.delete"),
        destructive: true,
      })
    ) {
      await del.mutateAsync(r.id);
      toast.success(t("common.delete"));
    }
  }

  async function toggle(r: RoutingRule) {
    try {
      await update.mutateAsync({ id: r.id, body: { ...bodyOf(r, r.priority), enabled: !r.enabled } });
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  }

  // Reordering swaps priorities. Equal priorities (common when rules were
  // created by hand or by an import) are normalised first, otherwise the swap
  // would be a no-op.
  async function move(r: RoutingRule, dir: -1 | 1) {
    const idx = sorted.findIndex((x) => x.id === r.id);
    const j = idx + dir;
    if (idx < 0 || j < 0 || j >= sorted.length) return;
    try {
      const normalized = sorted.map((x, i) => ({ rule: x, priority: i + 1 }));
      if (sorted.some((x, i) => x.priority !== i + 1)) {
        for (const n of normalized) {
          if (n.rule.priority !== n.priority) {
            await update.mutateAsync({ id: n.rule.id, body: bodyOf(n.rule, n.priority) });
          }
        }
      }
      const a = normalized[idx];
      const b = normalized[j];
      await update.mutateAsync({ id: a.rule.id, body: bodyOf(a.rule, b.priority) });
      await update.mutateAsync({ id: b.rule.id, body: bodyOf(b.rule, a.priority) });
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  }

  const canSubmit = matcherCount(f) > 0 && !!f.target && !create.isPending && !update.isPending;

  return (
    <div className="space-y-5 animate-page-enter">
      <div className="flex flex-col lg:flex-row lg:items-center justify-between gap-3">
        <div>
          <h1 className="text-2xl font-bold text-fg tracking-tight">{t("nav.routing")}</h1>
          <p className="text-sm text-fg-muted mt-1">
            {t("x.subtitleRouting")}
          </p>
        </div>
        <div className="flex items-center gap-2 flex-shrink-0">
          <NodePicker value={node} onChange={setNode} />
          <Button onClick={openNew} disabled={!node}>
            <Plus size={14} /> {t("common.add")}
          </Button>
        </div>
      </div>

      {!node ? (
        <GlassCard className="!p-8 text-center text-sm text-fg-muted">
          {t("x.pickNode")}
        </GlassCard>
      ) : (
        <GlassCard glow className="!p-0 overflow-hidden">
          <div className="divide-y divide-border/20">
            {sorted.map((r, i) => {
              const summary = matcherSummary(r);
              return (
                <div
                  key={r.id}
                  className={`flex items-center justify-between gap-3 px-5 py-3 text-sm ${
                    r.enabled ? "hover:bg-surface-2/40" : "opacity-55"
                  }`}
                >
                  <div className="flex min-w-0 items-center gap-3">
                    <span className="grid h-6 w-6 flex-shrink-0 place-items-center rounded-md bg-surface-2 text-xs text-fg-muted">
                      {i + 1}
                    </span>
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <span className="truncate font-medium text-fg">{r.name || "rule"}</span>
                        <span className="text-fg-subtle">→</span>
                        <Badge color={r.balancer_tag ? "on_hold" : "muted"}>
                          {r.outbound_tag || r.balancer_tag || "—"}
                        </Badge>
                        {!r.enabled && <Badge color="warning">{t("x.disabled")}</Badge>}
                      </div>
                      <div className="mt-1 flex flex-wrap items-center gap-1">
                        {summary.length === 0 ? (
                          <span className="text-xs text-amber-300">{t("x.matchesNothing")}</span>
                        ) : (
                          summary.map((m) => (
                            <span
                              key={m}
                              className="rounded border border-border/60 px-1.5 py-0.5 text-[11px] text-fg-muted"
                            >
                              {m}
                            </span>
                          ))
                        )}
                      </div>
                    </div>
                  </div>
                  <div className="flex flex-shrink-0 items-center gap-1">
                    <Button
                      variant="ghost"
                      size="sm"
                      title="Move up"
                      disabled={i === 0}
                      onClick={() => void move(r, -1)}
                    >
                      <ArrowUp size={15} />
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      title="Move down"
                      disabled={i === sorted.length - 1}
                      onClick={() => void move(r, 1)}
                    >
                      <ArrowDown size={15} />
                    </Button>
                    <Switch checked={r.enabled} onCheckedChange={() => void toggle(r)} />
                    <Button variant="ghost" size="sm" onClick={() => edit(r)}>
                      <Pencil size={15} />
                    </Button>
                    <Button variant="ghost" size="sm" className="text-danger" onClick={() => void remove(r)}>
                      <Trash2 size={15} />
                    </Button>
                  </div>
                </div>
              );
            })}
            {sorted.length === 0 && (
              <div className="flex flex-col items-center gap-2 px-5 py-10 text-center">
                <Waypoints size={22} className="text-fg-subtle" />
                <p className="text-sm text-fg-muted">{t("x.noRules")}</p>
                <p className="text-xs text-fg-subtle">
                  {t("x.noRulesHint")}
                </p>
              </div>
            )}
          </div>
        </GlassCard>
      )}

      <Modal
        open={open}
        onClose={() => {
          setOpen(false);
          setEditing(null);
        }}
        title={editing ? `Edit ${editing.name || "rule"}` : "New routing rule"}
        size="lg"
      >
        <form onSubmit={submit} className="space-y-3">
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
            <Input className="sm:col-span-2" placeholder="Name" value={f.name} onChange={set("name")} />
            <Input
              placeholder="Priority"
              value={f.priority}
              onChange={set("priority")}
              inputMode="numeric"
            />
          </div>

          <div>
            <div className="mb-1 text-xs font-medium text-fg-muted">
              {t("x.inboundsLabel")}
            </div>
            <div className="flex flex-wrap gap-1.5">
              {inboundTags.length === 0 && (
                <span className="text-xs text-fg-subtle">{t("x.noInbounds")}</span>
              )}
              {inboundTags.map((tag) => (
                <button
                  key={tag}
                  type="button"
                  onClick={() => toggleList("inbound_tags", tag)}
                  className={`rounded-md border px-2 py-1 text-xs transition-colors ${
                    f.inbound_tags.includes(tag)
                      ? "border-primary/60 bg-primary/15 text-fg"
                      : "border-border/60 text-fg-muted hover:text-fg"
                  }`}
                >
                  {tag}
                </button>
              ))}
            </div>
          </div>

          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
            <Input
              placeholder="Domains (comma, supports domain:/geosite:)"
              value={f.domains}
              onChange={set("domains")}
            />
            <Input placeholder="IP / CIDR (comma)" value={f.ip} onChange={set("ip")} />
          </div>

          <div>
            <div className="mb-1 text-xs font-medium text-fg-muted">{t("x.protocolLabel")}</div>
            <div className="flex flex-wrap gap-1.5">
              {PROTOCOLS.map((p) => (
                <button
                  key={p}
                  type="button"
                  onClick={() => toggleList("protocols", p)}
                  className={`rounded-md border px-2 py-1 text-xs transition-colors ${
                    f.protocols.includes(p)
                      ? "border-primary/60 bg-primary/15 text-fg"
                      : "border-border/60 text-fg-muted hover:text-fg"
                  }`}
                >
                  {p}
                </button>
              ))}
            </div>
          </div>

          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
            <Input placeholder='Port ("443" / "1-1000")' value={f.port} onChange={set("port")} />
            <Select value={f.network} onChange={set("network")}>
              {NETWORKS.map((n) => (
                <option key={n.value} value={n.value}>
                  {n.label}
                </option>
              ))}
            </Select>
          </div>

          <Select value={f.target} onChange={set("target")} required>
            <option value="">{t("x.targetPlaceholder")}</option>
            {f.target && !targetOptions.includes(f.target) && (
              <option value={f.target}>{f.target} (not on this node)</option>
            )}
            {targetOptions.map((o) => (
              <option key={o} value={o}>
                {o.startsWith("bal:") ? `${o.slice(4)} — balancer` : o.slice(4)}
              </option>
            ))}
          </Select>

          <div className="flex items-center justify-between">
            <Switch
              checked={f.enabled}
              onCheckedChange={(v) => setF((s) => ({ ...s, enabled: v }))}
              label={t("x.enabled")}
            />
            <span
              className={`text-xs ${matcherCount(f) === 0 ? "text-amber-300" : "text-fg-subtle"}`}
            >
              {matcherCount(f) === 0
                ? "needs at least one matcher"
                : `${matcherCount(f)} matcher(s)`}
            </span>
          </div>

          <div className="flex justify-end gap-2 pt-1">
            <Button
              type="button"
              variant="ghost"
              onClick={() => {
                setOpen(false);
                setEditing(null);
              }}
            >
              {t("common.cancel")}
            </Button>
            <Button type="submit" disabled={!canSubmit}>
              {editing ? t("common.save") : t("common.create")}
            </Button>
          </div>
        </form>
      </Modal>
    </div>
  );
}
