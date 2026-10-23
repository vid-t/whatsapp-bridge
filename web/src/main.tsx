import { createRoot } from "react-dom/client";
import "@fontsource-variable/geist";
import "@fontsource-variable/geist-mono";
import "./styles.css";
import { App } from "./app";

// No StrictMode: its double-mounted effects would start and cancel a real
// WhatsApp pairing on every load of the link page.
createRoot(document.getElementById("root")!).render(<App />);
