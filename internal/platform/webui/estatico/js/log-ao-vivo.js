/*
 * Teto de linhas do log ao vivo.
 *
 * A extensão sse do htmx insere cada mensagem no topo da lista e não tem
 * modificador de "no máximo N filhos": uma aba deixada aberta a noite inteira
 * acumularia dezenas de milhares de nós e o navegador engasgaria. Este arquivo
 * é a única linha de JavaScript escrita à mão na UI, e existe só por isso.
 *
 * Fica num arquivo servido do embed.FS, e não inline no HTML, para o layout
 * continuar sem nenhum <script> de corpo — o dia em que houver Content-Security
 * -Policy, uma diretiva de script-src resolve tudo de uma vez.
 */
(function () {
    "use strict";

    document.addEventListener("htmx:sseMessage", function (evento) {
        var lista = evento.target;
        if (!lista || typeof lista.getAttribute !== "function") {
            return;
        }
        // O placeholder "esperando a próxima..." só faz sentido enquanto a
        // lista está vazia: a primeira mensagem de verdade o torna obsoleto, e
        // sse-swap="afterbegin" não o remove sozinho — ele só insere.
        var vazio = lista.querySelector("[data-vazio]");
        if (vazio) {
            vazio.remove();
        }
        var limite = parseInt(lista.getAttribute("data-limite"), 10);
        if (!limite || limite < 1) {
            return;
        }
        // A mensagem nova entra em afterbegin, então o que sobra de velho está
        // no fim: remover pelo fim é remover o mais antigo.
        while (lista.children.length > limite) {
            lista.removeChild(lista.lastElementChild);
        }
    });
})();
