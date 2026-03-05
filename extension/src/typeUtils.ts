export function extractMapTypes(typeStr: string): { keyType: string; elemType: string } | null {
    if (!typeStr || !typeStr.startsWith('map[')) return null;
    let depth = 0;
    for (let i = 4; i < typeStr.length; i++) {
        if (typeStr[i] === '[') depth++;
        else if (typeStr[i] === ']') {
            if (depth === 0) {
                return {
                    keyType: typeStr.slice(4, i),
                    elemType: typeStr.slice(i + 1)
                };
            }
            depth--;
        }
    }
    return null;
}
