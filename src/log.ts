export function log(scope: string, message: string): void {
  const time = new Date().toISOString().slice(11, 19);
  console.log(`${time} [${scope}] ${message}`);
}
