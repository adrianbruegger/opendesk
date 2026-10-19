// What of the customer's conversation goes along when a side conversation starts
// (contracts/api.md "Threads"): nothing unless picked in the send menu, and what is picked
// shows next to the uploads, where it can be taken out again.

import { useQuery } from "@tanstack/react-query";
import { Check, FileText, Quote, X } from "lucide-react";
import { DropdownMenu as M } from "radix-ui";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { Attachment, Message } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { bytes } from "@/lib/format";
import { q } from "@/lib/queries";

/** Messages the first email can quote: the server takes the last 10. */
const MAX_QUOTED = 10;

/** The customer's conversation: public replies that reached them or came from them. */
function conversationOf(messages: Message[] | undefined): Message[] {
  return (messages ?? []).filter(
    (m) =>
      m.kind === "reply" &&
      !m.thread_id &&
      !m.id.startsWith("tmp-") &&
      (m.direction === "inbound" ? !m.auth_failed : m.delivery_status === "sent" || m.delivery_status === "none"),
  );
}

export type SideInclude = {
  /** How many messages the conversation would quote; 0 when there is none. */
  quoted: number;
  /** The files of the conversation, oldest first. */
  files: Attachment[];
  history: boolean;
  /** The picked files, as attachment ids. */
  picked: string[];
  onChange: (p: { threadHistory?: boolean; threadFiles?: string[] }) => void;
};

/** What a new side conversation on the ticket could take along, and what is picked. */
export function useSideInclude(
  ticketId: string,
  history: boolean,
  picked: string[],
  onChange: SideInclude["onChange"],
): SideInclude {
  const { data: messages } = useQuery(q.messages(ticketId));
  const conversation = conversationOf(messages);
  const files: Attachment[] = [];
  for (const m of conversation) for (const a of m.attachments) if (!files.some((x) => x.id === a.id)) files.push(a);
  return {
    quoted: Math.min(conversation.length, MAX_QUOTED),
    files,
    history: history && conversation.length > 0,
    picked: picked.filter((id) => files.some((a) => a.id === id)),
    onChange,
  };
}

/** The tooltip's words for what goes along, or "" for nothing. */
export function useIncludeSummary(inc: SideInclude | undefined): string {
  const { t } = useTranslation("inbox");
  if (!inc) return "";
  const n = inc.picked.length;
  if (inc.history && n) return t("side.withBoth", { count: n });
  if (inc.history) return t("side.withConversation");
  if (n) return t("side.withFiles", { count: n });
  return "";
}

/** The send menu's "Include" section. Picking keeps the menu open. */
export function SideIncludeItems({ inc }: { inc: SideInclude }) {
  const { t } = useTranslation("inbox");
  const toggle = (id: string) => inc.onChange({ threadFiles: inc.picked.includes(id) ? inc.picked.filter((x) => x !== id) : [...inc.picked, id] });
  return (
    <>
      <div className="label-caps px-2 pb-1 pt-1.5">{t("side.include")}</div>
      {inc.quoted > 0 && (
        <IncludeItem
          checked={inc.history}
          onToggle={() => inc.onChange({ threadHistory: !inc.history })}
          icon={<Quote />}
          label={t("side.conversation")}
          meta={t("side.messages", { count: inc.quoted })}
        />
      )}
      {inc.files.length > 1 && (
        <IncludeItem
          checked={inc.picked.length === inc.files.length}
          onToggle={() => inc.onChange({ threadFiles: inc.picked.length === inc.files.length ? [] : inc.files.map((a) => a.id) })}
          icon={<FileText />}
          label={t("side.allFiles")}
          meta={String(inc.files.length)}
        />
      )}
      <div className={cn("max-h-[184px] overflow-y-auto", inc.files.length > 1 && "pl-4")}>
        {inc.files.map((a) => (
          <IncludeItem key={a.id} checked={inc.picked.includes(a.id)} onToggle={() => toggle(a.id)} icon={<FileText />} label={a.filename} meta={bytes(a.size_bytes)} mask />
        ))}
      </div>
    </>
  );
}

function IncludeItem({ checked, onToggle, icon, label, meta, mask }: { checked: boolean; onToggle: () => void; icon: ReactNode; label: string; meta: string; mask?: boolean }) {
  return (
    <M.CheckboxItem
      checked={checked}
      onSelect={(e) => {
        e.preventDefault();
        onToggle();
      }}
      className="flex h-8 cursor-default items-center gap-2 rounded-md px-2 text-[12.5px] outline-none data-[highlighted]:bg-hover [&_svg]:size-3.5"
    >
      <span
        aria-hidden
        className={cn(
          "flex size-[15px] shrink-0 items-center justify-center rounded-sm border transition-colors",
          checked ? "border-inverse bg-inverse text-inverse-fg" : "border-line-strong bg-surface",
        )}
      >
        {checked && <Check className="!size-2.5" strokeWidth={3.5} />}
      </span>
      <span className="shrink-0 text-fg-4">{icon}</span>
      <span className="min-w-0 flex-1 truncate" data-ph-mask={mask || undefined}>
        {label}
      </span>
      <span className="mono shrink-0 pl-2 text-[10.5px] text-fg-4">{meta}</span>
    </M.CheckboxItem>
  );
}

/** What is picked, next to the uploads: each can be taken out again. */
export function SideIncludedChips({ inc }: { inc: SideInclude }) {
  const { t } = useTranslation("inbox");
  const files = inc.files.filter((a) => inc.picked.includes(a.id));
  return (
    <>
      {inc.history && (
        <Chip
          icon={<Quote className="size-3 text-fg-3" />}
          title={t("side.conversation")}
          meta={t("side.messages", { count: inc.quoted })}
          remove={t("side.remove", { name: t("side.conversation") })}
          onRemove={() => inc.onChange({ threadHistory: false })}
        />
      )}
      {files.map((a) => (
        <Chip
          key={a.id}
          icon={<FileText className="size-3 text-fg-3" />}
          title={a.filename}
          meta={t("side.fromCustomer", { size: bytes(a.size_bytes) })}
          remove={t("side.remove", { name: a.filename })}
          onRemove={() => inc.onChange({ threadFiles: inc.picked.filter((x) => x !== a.id) })}
          mask
        />
      ))}
    </>
  );
}

function Chip({ icon, title, meta, remove, onRemove, mask }: { icon: ReactNode; title: string; meta: string; remove: string; onRemove: () => void; mask?: boolean }) {
  return (
    <span className="flex h-8 max-w-[240px] items-center gap-2 rounded-md border border-line bg-surface pl-1 pr-1 animate-pop-in motion-reduce:animate-none">
      <span className="flex size-6 shrink-0 items-center justify-center rounded-sm bg-hover">{icon}</span>
      <span className="min-w-0">
        <span className="block truncate text-[11.5px]" data-ph-mask={mask || undefined}>
          {title}
        </span>
        <span className="mono block truncate text-[10px] text-fg-4">{meta}</span>
      </span>
      <button type="button" aria-label={remove} onClick={onRemove} className="flex size-5 shrink-0 items-center justify-center rounded-sm text-fg-4 hover:bg-hover hover:text-fg">
        <X className="size-3" />
      </button>
    </span>
  );
}
