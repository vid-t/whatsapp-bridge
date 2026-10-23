import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/** cn joins class names and lets later Tailwind utilities win over earlier ones. */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export const enc = encodeURIComponent;

/** Two letters for an avatar: "Alex Morgan" -> "AM", "+1555…" -> "#". */
export function initials(name: string) {
  const words = name
    .replace(/[^\p{L}\p{N}\s]/gu, " ")
    .trim()
    .split(/\s+/)
    .filter(Boolean);
  if (words.length === 0) return "#";
  if (/^\d/.test(words[0])) return "#";
  const first = [...words[0]][0] ?? "";
  const last = words.length > 1 ? ([...words[words.length - 1]][0] ?? "") : ([...words[0]][1] ?? "");
  return (first + last).toUpperCase();
}
