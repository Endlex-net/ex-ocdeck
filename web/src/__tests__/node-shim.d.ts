/** 测试环境最小 node 模块类型垫片（项目未装 @types/node，仅声明用到的两个函数）。 */
declare module 'node:fs' {
  export function readFileSync(path: string, encoding: 'utf8'): string;
}
declare module 'node:url' {
  export function fileURLToPath(url: URL): string;
}
