/** @novnc/novnc 1.7 未附带类型声明；这里只声明控制台用到的部分。包 exports 只开放根入口（core/rfb.js）。 */
declare module '@novnc/novnc' {
  export interface RFBOptions {
    credentials?: { password?: string; target?: string; username?: string };
    repeaterID?: string;
    shared?: boolean;
    wsProtocols?: string[];
  }

  export default class RFB extends EventTarget {
    constructor(target: HTMLElement, urlOrChannel: string | WebSocket, options?: RFBOptions);
    background: string;
    clipViewport: boolean;
    resizeSession: boolean;
    scaleViewport: boolean;
    viewOnly: boolean;
    disconnect(): void;
    focus(): void;
    sendCtrlAltDel(): void;
  }
}
