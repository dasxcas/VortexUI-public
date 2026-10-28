import { useMemo, useState } from "react";
import { Download, Loader2, RefreshCw } from "lucide-react";
import { useCoreVersions, useInstallNodeCore, type CoreVersionRow } from "@/api/hooks";
import type { Node } from "@/api/types";
import { Badge, Button } from "@/components/ui";
import { Modal } from "@/components/Modal";
import { useConfirm } from "@/components/confirm";
import { useToast } from "@/components/toast";
import { cn } from "@/lib/utils";
import { useI18n } from "@/i18n/i18n";

// The health report carries the core's own version banner ("sing-box version
// 1.14.2"), so compare on the bare number.
function normalizeVersion(v?: string) {
  if (!v) return "";
  const m = v.match(/\d+\.\d+\.\d+[\w.\-]*/);
  return m ? m[0] : v.trim();
}

function sizeLabel(bytes?: number) {
  if (!bytes) return "";
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/**
 * CoreVersionsModal lets an operator move a node between core builds.
 *
 * It is deliberately narrow: only ready artifacts for this node's core and
 * architecture are offered, an unmetered build is called out before it can be
 * installed, and the confirmation says the engine restarts (a few seconds of
 * dropped connections).
 */
export function CoreVersionsModal({ node, onClose }: { node: Node | null; onClose: () => void }) {
  const { data, isLoading, refetch, isFetching } = useCoreVersions();
  const install = useInstallNodeCore();
  const { t } = useI18n();
  const confirm = useConfirm();
  const toast = useToast();
  const [busyId, setBusyId] = useState<string | null>(null);

  const core = node?.core ?? "";
  const arch = node?.health?.arch ?? "";
  const current = normalizeVersion(node?.health?.core_version);

  const rows = useMemo(() => {
    const all = (data?.versions ?? []).filter(
      (v) => v.core === core && v.status === "ready" && (!arch || v.arch === arch),
    );
    // One row per version: prefer the built artifact, since that is the metered one.
    const byVersion = new Map<string, CoreVersionRow>();
    for (const r of all) {
      const prev = byVersion.get(r.version);
      if (!prev || (r.source === "built" && prev.source !== "built")) byVersion.set(r.version, r);
    }
    return [...byVersion.values()].sort((a, b) =>
      a.version.localeCompare(b.version, undefined, { numeric: true }),
    ).reverse();
  }, [data, core, arch]);

  async function doInstall(row: CoreVersionRow) {
    if (!node) return;
    const target = `${row.core} ${row.version} (${row.arch})`;
    const warn = row.metered
      ? `The node will restart its core to ${target}. Connections drop for a few seconds.`
      : `This build CANNOT count traffic per user, so quotas stop working.\n\nInstall ${target} anyway?`;
    const ok = await confirm({
      title: row.metered ? "Install core version" : "Install a build without metering?",
      message: warn,
      confirmLabel: "Install",
      destructive: !row.metered,
    });
    if (!ok) return;
    setBusyId(row.id);
    try {
      await install.mutateAsync({
        nodeId: node.id,
        core: row.core,
        versionId: row.id,
        force: !row.metered || !row.sha256,
      });
      toast.success(`${row.core} ${row.version} installed`);
      await refetch();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : String(e));
    } finally {
      setBusyId(null);
    }
  }

  return (
    <Modal
      open={!!node}
      onClose={onClose}
      title={t("x.coresTitle").replace("{name}", node?.name ?? "")}
      size="lg"
    >
      <div className="space-y-4">
        <div className="flex items-center justify-between gap-3 rounded-lg border border-border/60 bg-secondary/30 px-3 py-2 text-sm">
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
            <span className="text-muted-foreground">{t("x.running")}</span>
            <span className="font-mono">{core || "unknown"} {current || "—"}</span>
            {arch && <span className="text-xs text-muted-foreground">arch {arch}</span>}
          </div>
          <Button variant="ghost" size="sm" onClick={() => void refetch()} disabled={isFetching}>
            <RefreshCw className={cn("h-3.5 w-3.5", isFetching && "animate-spin")} />
          </Button>
        </div>

        {data && !data.build_enabled && (
          <p className="rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-200">
            Building is off on this panel. Only vendor artifacts are listed, and they cannot meter traffic
            for sing-box.
          </p>
        )}

        {isLoading ? (
          <div className="py-8 text-center text-sm text-muted-foreground">Loading…</div>
        ) : rows.length === 0 ? (
          <div className="py-8 text-center text-sm text-muted-foreground">
            No ready artifacts for {core || "this core"}{arch ? ` on ${arch}` : ""}. Refresh the catalog first.
          </div>
        ) : (
          <div className="max-h-[380px] space-y-2 overflow-y-auto pr-1">
            {rows.map((r) => {
              const isCurrent = normalizeVersion(r.version) === current;
              return (
                <div
                  key={r.id}
                  className={cn(
                    "flex items-center justify-between gap-3 rounded-lg border px-3 py-2",
                    isCurrent ? "border-primary/40 bg-primary/5" : "border-border/60",
                  )}
                >
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="font-mono text-sm">{r.version}</span>
                      {isCurrent && (
                        <Badge color="success">{t("x.running")}</Badge>
                      )}
                      {r.source === "bundled" && <Badge color="success">{t("x.baseline")}</Badge>}
                      {!r.sha256 && <Badge color="muted">{t("x.unverified")}</Badge>}
                      {r.metered ? (
                        <Badge color="success">{t("x.metered")}</Badge>
                      ) : (
                        <Badge color="warning">{t("x.noMetering")}</Badge>
                      )}
                    </div>
                    <div className="mt-0.5 text-[11px] text-muted-foreground">
                      {r.source === "bundled"
                        ? "bundled in the panel · metering fallback"
                        : r.source === "built"
                          ? "built here, checksummed"
                          : "vendor archive"} · {r.arch}
                      {sizeLabel(r.size_bytes) ? ` · ${sizeLabel(r.size_bytes)}` : ""}
                    </div>
                  </div>
                  <Button
                    size="sm"
                    variant={isCurrent ? "ghost" : "primary"}
                    disabled={isCurrent || busyId !== null}
                    onClick={() => void doInstall(r)}
                  >
                    {busyId === r.id ? (
                      <Loader2 className="h-3.5 w-3.5 animate-spin" />
                    ) : (
                      <Download className="h-3.5 w-3.5" />
                    )}
                    {isCurrent ? t("x.installed") : t("x.install")}
                  </Button>
                </div>
              );
            })}
          </div>
        )}

        <p className="text-[11px] leading-relaxed text-muted-foreground">
          The agent downloads the artifact from this panel, checks its sha256, keeps the previous binary as
          a rollback target, and restarts the engine. If anything fails, the old binary is restored.
        </p>
      </div>
    </Modal>
  );
}
