import { SendIcon } from "lucide-react";
import { Badge } from "@/components/ui/badge";

/** Marks a key or an assistant that was allowed to send messages to ticked chats. */
export function CanSendBadge() {
  return (
    <Badge variant="warning" title="Allowed to send messages to ticked chats while the bridge has sending on">
      <SendIcon aria-hidden="true" />
      Can send
    </Badge>
  );
}
