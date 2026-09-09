/**
 * 内联 SVG 图标组件（tasks 2.3）。
 * 规格（brand-spec）：1.6px 描边、单色 currentColor、不引图标库。
 * 替换原文本字符图标：▶ ⏸ ▼ ▲ ⟳ ⋯ ▸ ▾ ⚠ ⓘ ⎇ 等。
 *
 * 所有图标 24x24 viewBox，描边 1.6px，stroke=currentColor，fill=none，
 * 默认通过 className 跟随父元素字号/颜色；尺寸由 CSS 控制（width/height: 1em 或显式类）。
 * 通过 aria-hidden 默认对辅助技术隐藏，文字标签由调用方提供（title/aria-label）。
 */

interface IconProps {
  className?: string;
  /** 显式无障碍标签，不传则 aria-hidden */
  title?: string;
}

function Svg({ className, title, children }: IconProps & { children: React.ReactNode }) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      width="1em"
      height="1em"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.6}
      strokeLinecap="round"
      strokeLinejoin="round"
      role={title ? 'img' : undefined}
      aria-label={title}
      aria-hidden={title ? undefined : true}
      focusable="false"
    >
      {title ? <title>{title}</title> : null}
      {children}
    </svg>
  );
}

/** 激活 ▶ */
export function PlayIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M7 5.5L18 12 7 18.5z" />
    </Svg>
  );
}

/** 挂起 ⏸ */
export function PauseIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M9 5v14M15 5v14" />
    </Svg>
  );
}

/** 归档/收起 ▼ */
export function ArchiveIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M4 7h16v3H4zM5 10v9h14v-9M10 14h4" />
    </Svg>
  );
}

/** 恢复/展开 ▲ */
export function RestoreIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M12 5l6 6M12 5l-6 6M12 5v14" />
    </Svg>
  );
}

/** 重试 ⟳ */
export function RetryIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M4 12a8 8 0 1 1 2.5 5.8M4 18v-4h4" />
    </Svg>
  );
}

/** 更多 ⋯ */
export function MoreIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <circle cx="6" cy="12" r="1.2" fill="currentColor" stroke="none" />
      <circle cx="12" cy="12" r="1.2" fill="currentColor" stroke="none" />
      <circle cx="18" cy="12" r="1.2" fill="currentColor" stroke="none" />
    </Svg>
  );
}

/** 展开标记 ▸（折叠态右侧三角） */
export function CaretRightIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M9 6l6 6-6 6" />
    </Svg>
  );
}

/** 展开标记 ▾（展开态下向三角） */
export function CaretDownIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M6 9l6 6 6-6" />
    </Svg>
  );
}

/** 警告 ⚠ */
export function WarnIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M12 4l9 16H3z" />
      <path d="M12 10v5M12 18v.5" />
    </Svg>
  );
}

/** 信息 ⓘ */
export function InfoIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 8v.5M12 11v6" />
    </Svg>
  );
}

/** 分支 ⎇ */
export function BranchIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <circle cx="6" cy="6" r="2.5" />
      <circle cx="6" cy="18" r="2.5" />
      <circle cx="17" cy="8" r="2.5" />
      <path d="M6 8.5v7M17 10.5c0 4-5 3-5 6" />
    </Svg>
  );
}

/** 主题-暗色（月亮） */
export function MoonIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M20 14a8 8 0 1 1-10-10 6.5 6.5 0 0 0 10 10z" />
    </Svg>
  );
}

/** 主题-亮色（太阳） */
export function SunIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <circle cx="12" cy="12" r="4" />
      <path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M18.4 5.6l-2.1 2.1M7.7 16.3l-2.1 2.1" />
    </Svg>
  );
}

/* 壳层导航图标（侧栏 / 命令面板，设计源 command-center.html） */

/** 指挥中心（活动条/折线图） */
export function HomeIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <polyline points="3 12 8 12 11 5 14 19 17 12 21 12" />
    </Svg>
  );
}

/** 项目管理（文件夹） */
export function FolderIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7z" />
    </Svg>
  );
}

/** 设置（滑块） */
export function SettingsIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <line x1="4" y1="7" x2="20" y2="7" />
      <circle cx="9" cy="7" r="2.2" />
      <line x1="4" y1="17" x2="20" y2="17" />
      <circle cx="15" cy="17" r="2.2" />
    </Svg>
  );
}

/** 命令面板入口（放大镜） */
export function SearchIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <circle cx="11" cy="11" r="7" />
      <path d="m20 20-3.5-3.5" />
    </Svg>
  );
}

/** 新建（加号） */
export function PlusIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <line x1="12" y1="5" x2="12" y2="19" />
      <line x1="5" y1="12" x2="19" y2="12" />
    </Svg>
  );
}

/** 折叠侧栏（箭头指向左） */
export function SidebarCollapseIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <rect x="3" y="4" width="18" height="16" rx="2" />
      <path d="M9 4v16" />
      <path d="m14 9-3 3 3 3" />
    </Svg>
  );
}

/** 展开侧栏（箭头指向右） */
export function SidebarExpandIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <rect x="3" y="4" width="18" height="16" rx="2" />
      <path d="M9 4v16" />
      <path d="m12 9 3 3-3 3" />
    </Svg>
  );
}

/* 常用工具快捷打开图标（add-frontend-tool-quick-open 2.2，设置页/页头入口共用）
 * 例外于文件头单色线稿规格：此处为官方品牌标志形态（path 取自 simple-icons
 * visualstudiocode@12.4.0 / goland@16.30.0，viewBox 0 0 24 24 与本文件一致，无需归一化），
 * 实心填充、stroke=none、fillRule=evenodd 保证镂空窗口穿透。 */

/** VSCode（官方蓝色折带标志，品牌蓝 #007ACC；内三角窗口镂空） */
export function VSCodeIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path
        d="M23.15 2.587L18.21.21a1.494 1.494 0 0 0-1.705.29l-9.46 8.63-4.12-3.128a.999.999 0 0 0-1.276.057L.327 7.261A1 1 0 0 0 .326 8.74L3.899 12 .326 15.26a1 1 0 0 0 .001 1.479L1.65 17.94a.999.999 0 0 0 1.276.057l4.12-3.128 9.46 8.63a1.492 1.492 0 0 0 1.704.29l4.942-2.377A1.5 1.5 0 0 0 24 20.06V3.939a1.5 1.5 0 0 0-.85-1.352zm-5.146 14.861L10.826 12l7.178-5.448v10.896z"
        fill="#007ACC"
        stroke="none"
        fillRule="evenodd"
      />
    </Svg>
  );
}

/** Cursor（VSCode fork；官方 logo 为立体方块线框，此处以等距立方体剪影呈现，
 *  单色 currentColor 随主题取色，保持品牌形态可辨识。） */
export function CursorIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M12 2.5 3.5 7v10L12 21.5 20.5 17V7L12 2.5z" />
      <path d="M3.5 7 12 11.5 20.5 7" />
      <path d="M12 11.5v10" />
    </Svg>
  );
}

/** 自定义工具（通用外部唤起字形：方框 + 外指箭头，表达「经 URL scheme 唤起外部工具」）。 */
export function GenericToolIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M14 4h6v6" />
      <path d="M20 4l-9 9" />
      <path d="M11 5H5a1 1 0 0 0-1 1v13a1 1 0 0 0 1 1h13a1 1 0 0 0 1-1v-6" />
    </Svg>
  );
}

/** GoLand（JetBrains 官方方形标志：方底 + GO 字形 + 底部横条；GO 镂空透出底色。
 *  GoLand 官方 logo 底为近黑色方块，暗色主题下会融没，故用 currentColor 单色剪影
 *  随主题取色（亮主题=黑底白字、暗主题=反转），保持品牌形态可辨识。） */
export function GoLandIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path
        d="M0 0v24h24V0Zm6.764 3a5.448 5.448 0 0 1 3.892 1.356L9.284 6.012A3.652 3.652 0 0 0 6.696 5c-1.6 0-2.844 1.4-2.844 3.08v.028c0 1.812 1.244 3.14 3 3.14a3.468 3.468 0 0 0 2.048-.596V9.228H6.708v-1.88H11v4.296a6.428 6.428 0 0 1-4.228 1.572c-3.076 0-5.196-2.164-5.196-5.092v-.028A5.08 5.08 0 0 1 6.764 3Zm10.432 0c3.052 0 5.244 2.276 5.244 5.088v.028a5.116 5.116 0 0 1-5.272 5.12c-3.056-.02-5.248-2.296-5.248-5.112v-.028A5.116 5.116 0 0 1 17.196 3Zm-.028 2A2.96 2.96 0 0 0 14.2 8.068v.028a3.008 3.008 0 0 0 3 3.112 2.96 2.96 0 0 0 2.964-3.084v-.028A3.004 3.004 0 0 0 17.168 5ZM2.252 19.5h9V21h-9z"
        fill="currentColor"
        stroke="none"
        fillRule="evenodd"
      />
    </Svg>
  );
}
